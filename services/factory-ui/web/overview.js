"use strict";
// Overview is the factory as it is now: what waits for a person, what runs (each run as its live
// graph), the last 24 hours, and the services. It has no controls: it refreshes itself every 5 s
// while open. The measurement explorer with its periods is the Metrics page.
const OverviewLogic = (() => {
  const HOUR = 3600000;

  // hours sorts the runs started in the 24 hours before now into 24 hourly buckets, oldest
  // first, each counting its runs by state: { start, done, failed, running, total }.
  function hours(runs, stateOf, now) {
    const first = Math.floor(now / HOUR) * HOUR - 23 * HOUR;
    const buckets = Array.from({ length: 24 }, (_, i) => ({ start: first + i * HOUR, done: 0, failed: 0, running: 0, total: 0 }));
    for (const r of runs) {
      const at = Date.parse(r.started);
      if (!Number.isFinite(at) || at < first || at > now) continue;
      const b = buckets[Math.min(23, Math.floor((at - first) / HOUR))];
      const state = stateOf(r);
      if (state === "failed") b.failed++;
      else if (state === "running" || state === "waiting") b.running++;
      else if (state === "done") b.done++;
      b.total++;
    }
    return buckets;
  }

  // failureRate is the share of the finished runs in the buckets that failed, with its counts.
  function failureRate(buckets) {
    const failed = buckets.reduce((n, b) => n + b.failed, 0), finished = buckets.reduce((n, b) => n + b.failed + b.done, 0);
    return { failed, finished, percent: finished ? Math.round((failed / finished) * 100) : null };
  }

  // costHours adds up a cost series ({ series: [{ points: [{ time, value }] }] }) per hour of
  // the last 24, oldest first, with the total.
  function costHours(data, now) {
    const first = Math.floor(now / HOUR) * HOUR - 23 * HOUR;
    const sums = Array(24).fill(0);
    for (const line of data?.series || []) for (const p of line.points || []) {
      const at = Date.parse(p.time), value = Number(p.value);
      if (!Number.isFinite(at) || !Number.isFinite(value) || at < first || at > now) continue;
      sums[Math.min(23, Math.floor((at - first) / HOUR))] += value;
    }
    return { sums, total: sums.reduce((a, b) => a + b, 0) };
  }

  // stepState is the state of a step of a running run on its live graph: done once it left
  // its result, waiting while a person is asked (a gate or a session), running once every step
  // it depends on is done, else not started.
  function stepState(def, status, trace, decisions, sessions, id) {
    const artifacts = status?.artifacts || {};
    if (artifacts[id]) return "done";
    if (decisions.some((d) => d.trace_id === trace && (d.step === id || String(d.step || "").startsWith(`${id}.`)))) return "waiting";
    if (sessions.some((s) => String(s.session || "").startsWith(`user-sessions/${trace}/${id}`))) return "waiting";
    const s = (def?.steps || []).find((x) => x.id === id);
    if (s && (s.depends_on || []).every((d) => artifacts[d])) return "running";
    return "idle";
  }

  // needsYou lists what waits for a person or is broken, each with how many and where to go.
  function needsYou({ decisions, sessions, failed, services }) {
    const down = services.filter((s) => s.state !== "ready");
    return [
      { key: "decisions", label: decisions.length === 1 ? "Decision waits" : "Decisions wait", count: decisions.length, state: "waiting", href: "#decisions" },
      { key: "sessions", label: sessions.length === 1 ? "Session waits" : "Sessions wait", count: sessions.length, state: "waiting", href: "#sessions" },
      { key: "failed", label: failed === 1 ? "Run failed in 24 h" : "Runs failed in 24 h", count: failed, state: "failed", href: "#runs?status=failed" },
      { key: "services", label: down.length === 1 ? "Service not ready" : "Services not ready", count: down.length, state: "failed", href: "#cluster" },
    ];
  }

  return { HOUR, hours, failureRate, costHours, stepState, needsYou };
})();

(() => {
  const page = $("tab-overview");
  const O = OverviewLogic;
  const MAX_RUNNING = 6;
  const state = { active: false, timer: null, cost: null, costAt: 0, costError: "", updated: 0 };

  const needs = h("div", { className: "ov-needs" });
  const waiting = h("ul", { className: "ov-waiting" });
  const running = h("div", { className: "ov-running" });
  const charts = h("div", { className: "ov-charts" });
  const services = h("ul", { className: "ov-services" });
  const updated = h("span", { className: "ov-updated muted", role: "status" });
  const section = (title, ...body) => h("section", { className: "section ov-section" }, h("h3", { className: "section-title" }, title), ...body);
  page.replaceChildren(h("div", { className: "panel content-panel" },
    h("div", { className: "panel-head" }, h("h2", {}, "Live"), updated),
    h("div", { className: "panel-body ov-body", "aria-busy": "true" },
      h("section", { className: "section ov-section" }, needs, waiting),
      section("Running now", running),
      section("Last 24 hours", charts),
      section("Services", services))));
  const body = page.querySelector(".ov-body");

  // rowState is a run's state, waiting while one of its gates does.
  const rowState = (r) => runState(runIndex.statuses.get(r.trace_id)?.status, waitsForPerson(r.trace_id));
  // waitsForPerson: a gate or an interactive session of the run waits, as on Runs.
  const waitsForPerson = (trace) => decisionsState.pending.some((d) => d.trace_id === trace)
    || sessionsState.pending.some((s) => String(s.session || "").startsWith(`user-sessions/${trace}/`));

  async function loadCost(now) {
    if (now - state.costAt < 60000 && (state.cost || state.costError)) return; // once a minute
    state.costAt = now;
    const params = new URLSearchParams({ metric: "cost", group_by: "workflow", currency: localStorage.getItem("factory.displayCurrency") || "EUR", from: new Date(now - 24 * O.HOUR).toISOString(), to: new Date(now).toISOString() });
    try { state.cost = await api("GET", `/api/sqldb/series?${params}`); state.costError = ""; }
    catch (e) { state.cost = null; state.costError = e.message; }
  }

  async function refresh() {
    if (!state.active) return;
    const now = Date.now();
    try { await Promise.all([runIndex.refresh(), runIndex.loadWorkflowNames()]); } catch { /* the last listing stays */ }
    const recent = runIndex.list("").filter((r) => now - Date.parse(r.started) < 24 * O.HOUR);
    // A row needs the status alone; a finished run's is cached, a running one's asked again.
    await Promise.all([pool(recent.slice(0, 200), 6, (r) => runIndex.status(r.trace_id, 3000, "")), loadCost(now)]);
    if (!state.active) return;
    const live = recent.filter((r) => runIndex.statuses.get(r.trace_id)?.status === "Running").slice(0, MAX_RUNNING);
    // A running run's graph needs the outputs its steps stored, which only the run's listing has,
    // as on Runs: the status alone would show a finished step as still running.
    const [defs] = await Promise.all([
      Promise.all(live.map((r) => runWorkflow(r.workflow, Number(r.workflow_version) || 0))),
      pool(live, 3, (r) => runIndex.status(r.trace_id, 3000, "overview")),
    ]);
    if (!state.active) return;
    render(now, recent, live, defs);
  }

  function render(now, recent, live, defs) {
    const buckets = O.hours(recent, rowState, now);
    const rate = O.failureRate(buckets);
    // What needs a person: one tile each, a count and where to go.
    needs.replaceChildren(...O.needsYou({ decisions: decisionsState.pending, sessions: sessionsState.pending, failed: rate.failed, services: clusterHealth.services }).map((n) =>
      h("a", { className: `ov-need${n.count ? ` ov-need-${n.state}` : ""}`, href: n.href },
        h("span", { className: "ov-need-count" }, n.count ? String(n.count) : "None"),
        h("span", { className: "ov-need-label" }, n.count ? statusChip(n.state, n.label) : n.label))));
    // The gates that wait, newest first, each opening its decision.
    waiting.replaceChildren(...decisionsState.pending.slice(0, 5).map((d) => h("li", {},
      h("a", { href: `#decisions/${encodeURIComponent(`p:${d.workflow_id}|${d.step}`)}` },
        statusChip("waiting"), h("span", { className: "ov-waiting-name" }, `${titleCase(d.workflow || runIndex.records.get(d.trace_id)?.workflow || "")} › ${stepTitle(d.step)}`),
        d.question ? h("span", { className: "ov-waiting-question muted", title: d.question }, d.question) : null))));
    waiting.hidden = !decisionsState.pending.length;
    // The running runs, each as its live graph.
    running.replaceChildren(...(live.length ? live.map((r, i) => runCard(r, defs[i])) : [h("p", { className: "muted" }, "Nothing runs now.")]));
    charts.replaceChildren(runsChart(buckets, now), rateTile(rate), costTile(now));
    services.replaceChildren(...(clusterHealth.services.length ? clusterHealth.services.map((s) => h("li", {},
      statusChip(s.state === "ready" ? "done" : s.state === "starting" ? "running" : "failed", s.state === "ready" ? "Ready" : s.state === "starting" ? "Starting" : "Not ready"),
      h("span", { className: "ov-service-name" }, s.name), h("span", { className: "ov-service-count muted" }, `${s.ready}/${s.desired}`))) : [h("li", { className: "muted" }, clusterHealth.error ? `Cannot read the services: ${clusterHealth.error}` : "Reading the services…")]));
    body.setAttribute("aria-busy", "false");
    state.updated = now;
    updated.textContent = `Updated ${new Date(now).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`;
  }

  function runCard(r, def) {
    const st = runIndex.statuses.get(r.trace_id);
    const svg = document.createElementNS(WorkflowGraph.SVG_NS, "svg");
    svg.setAttribute("class", "run-graph");
    svg.setAttribute("aria-hidden", "true"); // the card's link names the run; the graph is its picture
    const card = h("a", { className: "ov-run", href: `#runs/${r.trace_id}` },
      h("span", { className: "ov-run-head" }, runChipFor(r), h("span", { className: "ov-run-name" }, titleCase(r.workflow || "")), h("span", { className: "muted" }, timeAgo(r.started))),
      def?.steps?.length ? svg : h("span", { className: "muted" }, "Graph unavailable"));
    if (def?.steps?.length) {
      const w = { ...def, steps: def.steps.map((s) => ({ ...s, position: s.position ? { ...s.position } : undefined })) };
      WorkflowGraph.autoLayout(w);
      const view = WorkflowGraph.fitView(w, 320, { input: false, minScale: 0.15, maxScale: 0.5, height: 160, margin: 8 });
      svg.setAttribute("width", 320);
      svg.setAttribute("height", 160);
      WorkflowGraph.drawGraph(svg, w, { view, idPrefix: `ov-${r.trace_id.slice(0, 8)}-`, stateOf: (id) => O.stepState(w, st, r.trace_id, decisionsState.pending, sessionsState.pending, id) });
      for (const t of svg.querySelectorAll(".tile")) t.removeAttribute("tabindex"); // one stop per run: its card
    }
    return card;
  }

  const runChipFor = (r) => {
    const st = runIndex.statuses.get(r.trace_id);
    const waits = waitsForPerson(r.trace_id);
    return statusChip(runState(st?.status, waits), runStateWord(st?.status, waits));
  };

  function timeAgo(iso) {
    const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
    return s < 60 ? `${s} s ago` : s < 3600 ? `${Math.floor(s / 60)} min ago` : `${Math.floor(s / 3600)} h ago`;
  }

  const svgNode = (tag, attrs = {}, text) => {
    const node = document.createElementNS(WorkflowGraph.SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, String(v));
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const hourLabel = (t) => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  // runsChart: the runs started per hour, each bar split by state (failed at the bottom).
  function runsChart(buckets, now) {
    const max = Math.max(1, ...buckets.map((b) => b.total));
    const W = 480, H = 120, bw = W / 24;
    const svg = svgNode("svg", { viewBox: `0 0 ${W} ${H + 18}`, role: "img", "aria-label": `Runs started per hour: ${buckets.reduce((n, b) => n + b.total, 0)} in 24 hours` });
    buckets.forEach((b, i) => {
      let y = H;
      for (const [key, cls] of [["failed", "failed"], ["running", "running"], ["done", "done"]]) {
        if (!b[key]) continue;
        const height = (b[key] / max) * (H - 8);
        y -= height;
        const bar = svgNode("rect", { x: i * bw + 2, y, width: bw - 4, height, class: `ov-bar ov-bar-${cls}` });
        bar.append(svgNode("title", {}, `${hourLabel(b.start)}: ${b.total} run${b.total === 1 ? "" : "s"}, ${b.failed} failed`));
        svg.append(bar);
      }
    });
    svg.append(svgNode("path", { d: `M0 ${H + .5} H${W}`, class: "plot-axis", fill: "none" }),
      svgNode("text", { x: 0, y: H + 14, class: "plot-label" }, hourLabel(buckets[0].start)),
      svgNode("text", { x: W, y: H + 14, "text-anchor": "end", class: "plot-label" }, "Now"));
    const legend = h("div", { className: "ov-legend" }, statusChip("done"), statusChip("running"), statusChip("failed"));
    return h("div", { className: "ov-chart ov-chart-runs" }, h("div", { className: "ov-chart-title" }, "Runs per hour"), svg, legend);
  }

  function rateTile(rate) {
    return h("div", { className: "ov-chart" }, h("div", { className: "ov-chart-title" }, "Failure rate"),
      h("div", { className: "ov-figure" }, rate.percent === null ? "None finished" : `${rate.percent}%`),
      h("div", { className: "muted" }, rate.finished ? `${rate.failed} of ${rate.finished} finished runs failed` : "No run finished in 24 hours"));
  }

  function costTile(now) {
    const currency = localStorage.getItem("factory.displayCurrency") || "EUR";
    if (!state.cost) return h("div", { className: "ov-chart" }, h("div", { className: "ov-chart-title" }, "Cost"), h("div", { className: "muted" }, state.costError ? `Cannot read the cost: ${state.costError}` : "Reading the cost…"));
    const { sums, total } = O.costHours(state.cost, now);
    const max = Math.max(...sums, 0);
    const W = 240, H = 48, bw = W / 24;
    const svg = svgNode("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": `Cost per hour, ${total.toFixed(2)} ${currency} in 24 hours` });
    if (max > 0) sums.forEach((v, i) => { if (v > 0) svg.append(svgNode("rect", { x: i * bw + 1, y: H - (v / max) * H, width: bw - 2, height: (v / max) * H, class: "ov-bar ov-bar-cost" })); });
    return h("div", { className: "ov-chart" }, h("div", { className: "ov-chart-title" }, "Cost"),
      h("div", { className: "ov-figure" }, `${total.toFixed(2)} ${currency}`), svg, h("a", { href: "#metrics" }, "View in Metrics"));
  }

  document.addEventListener("decisionschange", () => { if (state.active && state.updated) refresh(); });
  document.addEventListener("clusterchange", () => { if (state.active && state.updated) refresh(); });

  tabs.overview = {
    show() {
      if (state.active) return;
      state.active = true;
      refresh();
      state.timer = setInterval(() => { if (!document.hidden) refresh(); }, 5000);
    },
    hide() { state.active = false; clearInterval(state.timer); state.timer = null; },
  };
})();
