"use strict";
// Metrics: the factory over a period. In order: the period, four totals (cost, runs, runtime,
// tokens), one measurement compared across groups as a ranked table, and its trend over time,
// drawn only when a group has enough measured hours to form a line.
(() => {
  const body = measurementUI.panel("metrics", true), state = { data: null, totals: null, totalsKey: "", query: null, active: false, request: 0 };
  const METRICS = [["cost", "Cost"], ["duration_ms", "Runtime"], ["input_tokens", "Input tokens"], ["output_tokens", "Output tokens"], ["cached_input_tokens", "Cached input tokens"], ["uncached_input_tokens", "Uncached input tokens"], ["quality", "Quality metric"]];
  const GROUPS = { model: "Model", workflow: "Workflow", agent: "Agent", run: "Run" };
  const TREND_SERIES = 6;
  const metric = measurementUI.select("metric-name", [["", "Not set"], ...METRICS]);
  const quality = h("input", { id: "metric-quality", hidden: true });
  const grouping = measurementUI.select("metric-group", [["", "Not set"], ...Object.entries(GROUPS)]);
  const range = measurementUI.select("metric-range", [["24h", "Last 24 hours"], ["7d", "Last 7 days"], ["30d", "Last 30 days"], ["custom", "Custom range"]]);
  range.value = "7d";
  const from = h("input", { id: "metric-from", type: "datetime-local", required: true });
  const to = h("input", { id: "metric-to", type: "datetime-local", required: true });
  // Outcome narrows every number on the page to the runs that ended that way.
  const outcome = measurementUI.select("metric-outcome", [["", "All runs"], ["completed", "Completed"], ["failed", "Failed"]]);
  const currency = measurementUI.select("metric-currency", [["EUR", "EUR"], ["USD", "USD"]]);
  currency.value = localStorage.getItem("factory.displayCurrency") || "EUR";
  const status = h("p", { className: "metric-status muted", role: "status" });
  const tiles = h("div", { className: "metric-tiles" });
  const barsTitle = h("h2", {}), bars = h("div", { className: "metric-bars" });
  const chart = h("div", { className: "measurement-chart" }), trend = h("section", { className: "metric-trend", hidden: true }, h("h2", {}, "Trend"), chart);
  const findings = h("div", { className: "measurement-findings" });
  const qualityField = measurementUI.field("Quality metric name", quality); qualityField.hidden = true;
  const fromField = measurementUI.field("From", from), toField = measurementUI.field("To", to);
  fromField.hidden = toField.hidden = true;
  // Every preset and dropdown reloads at once; only typed dates wait for Apply.
  const submit = h("button", { type: "submit", className: "primary", hidden: true }, "Apply");

  // ---- Units: every number is shown with what it measures ----
  const kind = () => metric.value === "duration_ms" ? "duration" : metric.value === "cost" ? "cost" : metric.value === "quality" ? "plain" : "tokens";
  const compact = (v) => new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 }).format(v).replace("K", "k");
  function duration(ms) {
    const s = ms / 1000;
    if (s < 60) return `${s < 10 ? s.toFixed(1) : (Math.round(s * 10) / 10).toFixed(1)} s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m} min ${Math.round(s - m * 60)} s`;
    return `${Math.floor(m / 60)} h ${m % 60} min`;
  }
  const money = (v, code) => new Intl.NumberFormat(undefined, { style: "currency", currency: code, maximumFractionDigits: v !== 0 && Math.abs(v) < 1 ? 4 : 2 }).format(v);
  // format writes a value of a kind in full ("30.6 s", "€0.42", "12.4k tokens"); on an axis the word tokens is left out.
  function format(of, v, axis = false) {
    if (of === "duration") return duration(v);
    if (of === "cost") return money(v, state.query?.currency || currency.value);
    if (of === "tokens") return axis ? compact(v) : `${compact(v)} tokens`;
    return Number(Number(v).toPrecision(4)).toLocaleString();
  }
  const day = (t) => new Date(t).toLocaleDateString(undefined, { day: "numeric", month: "short" });
  const clock = (t) => new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  const moment = (t) => `${day(t)}, ${clock(t)}`;

  // ---- Groups: named as the rest of the factory names them, never by a bare UUID ----
  const unattributed = (key) => !key || key === "<nil>";
  const title = (id) => typeof titleCase === "function" ? titleCase(id) : id;
  const shorten = (id) => typeof midTruncate === "function" ? midTruncate(id, 13) : id;
  function groupName(key) {
    if (unattributed(key)) return "Unattributed";
    const by = state.query?.group;
    if (by === "run") return typeof runIndex !== "undefined" && runIndex.records?.get(key) ? `${runIndex.name(key)} · ${shorten(key)}` : shorten(key);
    return by === "workflow" || by === "agent" ? title(key) : key;
  }
  const valid = (p) => p.value !== null && p.value !== undefined && Number.isFinite(Number(p.value)) && Number.isFinite(Date.parse(p.time));
  const sum = (series) => (series || []).reduce((total, line) => total + (line.points || []).filter(valid).reduce((n, p) => n + Number(p.value), 0), 0);
  // total is what one group amounts to in the period: its sum, or for a quality score its average.
  function total(line) {
    const points = (line.points || []).filter(valid);
    if (!points.length) return null;
    const all = points.reduce((n, p) => n + Number(p.value), 0);
    return kind() === "plain" ? all / points.length : all;
  }

  function updateGroups() {
    quality.hidden = qualityField.hidden = metric.value !== "quality";
    const previous = grouping.value;
    const choices = metric.value === "duration_ms" ? [["workflow", "Workflow"], ["run", "Run"]]
      : metric.value === "quality" ? [["run", "Evaluated run"], ["agent", "Judge agent"]]
        : Object.entries(GROUPS);
    grouping.replaceChildren(h("option", { value: "" }, "Not set"), ...choices.map(([value, label]) => h("option", { value }, label)));
    grouping.value = choices.some(([value]) => value === previous) ? previous : "";
    grouping.title = metric.value === "duration_ms" ? "Whole-workflow runtime has no single model or agent attribution" : metric.value === "quality" ? "Groups findings by evaluated run or producing judge" : "";
  }
  function updateRange() {
    fromField.hidden = toField.hidden = submit.hidden = range.value !== "custom";
    if (range.value === "custom") return;
    const end = new Date(), days = range.value === "24h" ? 1 : range.value === "30d" ? 30 : 7;
    const local = date => new Date(+date - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    from.value = local(new Date(+end - days * 86400000)); to.value = local(end);
  }
  metric.onchange = () => {
    updateGroups();
    if (!grouping.value) grouping.value = metric.value === "duration_ms" ? "workflow" : metric.value === "quality" ? "run" : "model";
    if (state.active) load();
  };
  range.onchange = () => { updateRange(); if (state.active && range.value !== "custom") load(); };
  grouping.onchange = currency.onchange = outcome.onchange = () => { if (state.active) load(); };
  metric.value = "duration_ms"; metric.onchange(); updateRange();
  const form = h("form", { className: "measurement-filters", onsubmit: event => { event.preventDefault(); load(); } },
    measurementUI.field("Period", range), fromField, toField, measurementUI.field("Outcome", outcome),
    measurementUI.field("Measurement", metric), qualityField, measurementUI.field("Group by", grouping), measurementUI.field("Currency", currency), submit);
  // Every control sits in the one row at the top; what they choose follows under it.
  body.append(form, tiles, status, barsTitle, bars, trend, findings);

  function svgNode(tag, attrs = {}, text) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
    if (text !== undefined) node.textContent = text;
    return node;
  }
  const series = (path) => api("GET", `/api/sqldb/series?${path}`);

  async function load() {
    if (!metric.value || !grouping.value || (metric.value === "quality" && !quality.value.trim())) { status.replaceChildren("Select a measurement and grouping."); return; }
    const start = new Date(from.value), end = new Date(to.value);
    if (!Number.isFinite(+start) || !Number.isFinite(+end) || start >= end) { status.replaceChildren("Select a valid time range."); return; }
    const request = ++state.request; submit.disabled = true;
    state.query = { metric: metric.value, label: Object.fromEntries(METRICS)[metric.value], group: grouping.value, currency: currency.value, start: +start, end: +end };
    status.replaceChildren("Loading measurements…");
    bars.replaceChildren(); chart.replaceChildren(); trend.hidden = true; barsTitle.textContent = "";
    loadFindings(start, end, request);
    loadTotals(start, end, request);
    try {
      const params = new URLSearchParams({ metric: metric.value === "quality" ? quality.value.trim() : metric.value, group_by: grouping.value, from: start.toISOString(), to: end.toISOString() });
      if (metric.value === "cost") params.set("currency", currency.value);
      if (outcome.value) params.set("outcome", outcome.value);
      const data = await series(params);
      if (!state.active || request !== state.request) return;
      state.data = data; render();
    } catch (error) { if (request === state.request) status.replaceChildren(`Cannot load measurements: ${error.message}`); }
    finally { if (request === state.request) submit.disabled = false; }
  }

  // loadTotals reads what the tiles show for the period. It asks again only when the period, the
  // currency or the outcome changes, so choosing another measurement leaves the tiles as they are.
  async function loadTotals(start, end, request) {
    const key = `${start.toISOString()}|${end.toISOString()}|${currency.value}|${outcome.value}`;
    if (key === state.totalsKey && state.totals) return;
    state.totals = null; state.totalsKey = key; renderTiles();
    const ask = async (name, group, extra = {}) => {
      try { return await series(new URLSearchParams({ metric: name, group_by: group, from: start.toISOString(), to: end.toISOString(), ...(outcome.value ? { outcome: outcome.value } : {}), ...extra })); }
      catch (error) { return { error: error.message }; }
    };
    const [runs, cost, input, output] = await Promise.all([ask("duration_ms", "run"), ask("cost", "workflow", { currency: currency.value }), ask("input_tokens", "workflow"), ask("output_tokens", "workflow")]);
    if (!state.active || request !== state.request) { if (state.totalsKey === key) state.totalsKey = ""; return; }
    state.totals = { runs, cost, input, output, currency: currency.value };
    renderTiles(); renderStatus();
  }

  // A tile is one total of the period. A total with no measured value says Unknown, never 0,
  // and a total that lacks values says how many.
  function tile(label, value, missing, failed) {
    const known = value !== null;
    return h("div", { className: "metric-tile" },
      h("span", { className: `metric-tile-value${known ? "" : " muted"}` }, failed ? "Unavailable" : known ? value : "Unknown"),
      h("span", { className: "metric-tile-label" }, label),
      failed ? h("span", { className: "metric-tile-note", title: failed }, "Cannot be read")
        : missing ? h("span", { className: "metric-tile-note", title: "Measurements that were expected in this period and are not recorded" }, `${missing.toLocaleString()} ${missing === 1 ? "value" : "values"} missing`) : null);
  }
  function renderTiles() {
    const t = state.totals;
    if (!t) { tiles.replaceChildren(...["Cost", "Runs", "Runtime", "Tokens"].map((label) => tile(label, "…", 0))); return; }
    const has = (data) => !data.error && (data.series || []).some((line) => (line.points || []).some(valid));
    const missing = (...sets) => sets.reduce((n, data) => n + (Number(data.coverage?.missing) || 0), 0);
    const runCount = (t.runs.series || []).filter((line) => (line.points || []).some(valid)).length;
    const tokens = has(t.input) || has(t.output);
    tiles.replaceChildren(
      tile("Cost", has(t.cost) ? money(sum(t.cost.series), t.currency) : null, missing(t.cost), t.cost.error),
      tile("Runs", t.runs.error ? null : runCount.toLocaleString(), 0, t.runs.error),
      tile("Runtime", has(t.runs) ? duration(sum(t.runs.series)) : null, missing(t.runs), t.runs.error),
      tile("Tokens", tokens ? compact(sum(t.input.series) + sum(t.output.series)) : null, missing(t.input, t.output), t.input.error || t.output.error),
    );
    if (tokens) tiles.children[3].title = `Input ${compact(sum(t.input.series))} · Output ${compact(sum(t.output.series))}`;
  }

  // The status line says how much was measured, and warns only when something is missing.
  function renderStatus() {
    const data = state.data, t = state.totals;
    if (!data) return;
    const parts = [];
    const missing = Number(data.coverage?.missing) || 0;
    if (missing) parts.push(h("span", { className: "metric-warning", title: "Measurements that were expected in this period and are not recorded" }, `${missing.toLocaleString()} ${missing === 1 ? "value" : "values"} missing`));
    if (data.truncated) parts.push(h("span", { className: "metric-warning" }, "Results capped"));
    if (data.projection?.status && data.projection.status !== "complete") parts.push(h("span", { title: "The newest hours are still being added up" }, "Latest hours still counting"));
    status.replaceChildren(...parts.flatMap((part, i) => i ? [" · ", part] : [part]));
  }

  async function loadFindings(start, end, request, cursor = "", append = false) {
    try {
      const params = new URLSearchParams({ kind: "judge", from: start.toISOString(), to: end.toISOString(), limit: "100" });
      if (cursor) params.set("cursor", cursor);
      const data = await api("GET", `/api/sqldb/measurements?${params}`);
      if (!state.active || request !== state.request) return;
      const rows = measurementUI.rows(data).filter(record => record.text_value !== undefined || record.status === "invalid" || record.status === "missing");
      const nodes = rows.map(record => {
        const targetTrace = record.target_trace_id || record.target_run_id;
        const producerTrace = record.producer_trace_id || record.producer_run_id;
        return h("details", { className: "measurement-finding" },
          h("summary", {}, record.metric_name || "Finding"),
          h("p", {}, record.text_value ?? ((record.missing || []).join("; ") || "Unavailable")),
          h("div", { className: "measurement-actions" },
            targetTrace ? h("a", { href: `#runs/${encodeURIComponent(targetTrace)}` }, "Evaluated run") : null,
            producerTrace ? h("a", { href: `#runs/${encodeURIComponent(producerTrace)}` }, "Judge run") : null,
            record.artifact_id ? h("a", { href: memoryObjectHash(record.artifact_id) }, "JSON output") : null));
      });
      if (!append) findings.replaceChildren(h("h2", {}, "Findings in this time range"));
      findings.querySelector?.("button")?.remove();
      findings.append(...nodes);
      if (data.next_cursor) findings.append(h("button", { type: "button", onclick: () => loadFindings(start, end, request, data.next_cursor, true) }, "More findings"));
      if (!rows.length && !data.next_cursor && !append) findings.replaceChildren();
    } catch (error) { if (state.active && request === state.request) findings.replaceChildren(errorPanel(`Cannot load findings: ${error.message}`)); }
  }

  // niceStep rounds a step up to 1, 2, 2.5 or 5 times a power of ten.
  function niceStep(raw) {
    const power = 10 ** Math.floor(Math.log10(raw)), n = raw / power;
    return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10) * power;
  }
  // valueTicks are five round values from 0 up to at least max; runtime steps in whole seconds or minutes.
  function valueTicks(max, of) {
    const unit = of === "duration" ? (max >= 3600000 ? 3600000 : max >= 120000 ? 60000 : 1000) : 1;
    const step = niceStep((max || 1) / unit / 4) * unit;
    return [0, 1, 2, 3, 4].map((i) => i * step);
  }
  // timeTicks are the days across the period ("21 Nov"), or its hours ("14:00") when it is two days or less.
  function timeTicks(a, b) {
    const ticks = [], span = b - a, at = new Date(a);
    if (span <= 2 * 86400000) {
      const every = span <= 6 * 3600000 ? 1 : span <= 26 * 3600000 ? 3 : 6;
      at.setMinutes(0, 0, 0);
      while (+at < a || at.getHours() % every) at.setHours(at.getHours() + 1);
      for (; +at <= b; at.setHours(at.getHours() + every)) ticks.push({ time: +at, label: at.getHours() === 0 ? day(at) : clock(at) });
    } else {
      const every = Math.ceil(span / 86400000 / 8);
      at.setHours(0, 0, 0, 0);
      if (+at < a) at.setDate(at.getDate() + 1);
      for (; +at <= b; at.setDate(at.getDate() + every)) ticks.push({ time: +at, label: day(at) });
    }
    return ticks;
  }

  function render() {
    const data = state.data, q = state.query; if (!data || !q) return;
    const of = kind(), by = GROUPS[q.group] || q.group;
    renderStatus();
    barsTitle.textContent = `${q.label} by ${by.toLowerCase()}`;
    const ranked = (data.series || []).map((line) => ({ line, total: total(line) })).filter((row) => row.total !== null).sort((a, b) => b.total - a.total);
    if (!ranked.length) { bars.replaceChildren(emptyPanel("No measured values")); trend.hidden = true; return; }
    // The trend shows the largest groups, and only if one of them has two measured hours to join.
    const plotted = ranked.slice(0, TREND_SERIES);
    const drawTrend = plotted.some((row) => (row.line.points || []).filter(valid).length > 1);
    const top = Math.max(...ranked.map((row) => row.total), 0);
    // One ranked table, edge to edge like every table: the group, its total, and a bar of that
    // total beside it, so the number and its bar are read together at any width.
    bars.replaceChildren(dataTable([by, q.label, ""], ranked.map((row, index) => {
      const key = row.line.key, name = groupName(key);
      const label = q.group === "run" && !unattributed(key) ? h("a", { href: `#runs/${encodeURIComponent(key)}` }, name) : name;
      return h("tr", { className: "metric-bar-row" },
        h("td", { className: "metric-rank-name", title: unattributed(key) ? "Measurements that name no " + by.toLowerCase() : key },
          drawTrend && index < TREND_SERIES ? h("span", { className: `metric-swatch series-${index}`, "aria-hidden": "true" }) : null, label),
        h("td", { className: "num" }, format(of, row.total)),
        h("td", { className: "metric-rank-bar", "aria-hidden": "true" }, h("span", { className: "metric-bar", style: `width:${top ? Math.max(row.total / top * 100, .5) : 0}%` })));
    }), { numeric: [1] }));

    trend.hidden = !drawTrend;
    if (drawTrend) drawPlot(plotted, of, q);
    else chart.replaceChildren();
  }

  // drawPlot draws the trend one unit per pixel, so its labels keep the size of the page's text.
  function drawPlot(plotted, of, q) {
    const width = Math.max(chart.clientWidth || 960, 480), height = 280, left = 72, right = width - 16, bottom = height - 36, topY = 12;
    const values = plotted.flatMap((row) => (row.line.points || []).filter(valid));
    const yTicks = valueTicks(Math.max(...values.map((p) => Number(p.value)), 0), of), maxY = yTicks[yTicks.length - 1] || 1;
    const x = (t) => left + ((Date.parse(t) - q.start) / (q.end - q.start || 1)) * (right - left);
    const y = (v) => bottom - (Number(v) / maxY) * (bottom - topY);
    const svg = svgNode("svg", { viewBox: `0 0 ${width} ${height}`, height, class: "metric-plot", role: "img", "aria-label": `${q.label} over time by ${(GROUPS[q.group] || q.group).toLowerCase()}` });
    for (const value of yTicks) svg.append(svgNode("path", { d: `M${left} ${y(value)} H${right}`, class: "plot-grid", fill: "none" }), svgNode("text", { x: left - 8, y: y(value) + 4, "text-anchor": "end", class: "plot-label" }, format(of, value, true)));
    for (const tick of timeTicks(q.start, q.end)) {
      const at = left + ((tick.time - q.start) / (q.end - q.start || 1)) * (right - left);
      svg.append(svgNode("path", { d: `M${at} ${bottom} v4`, class: "plot-axis", fill: "none" }), svgNode("text", { x: at, y: bottom + 20, "text-anchor": "middle", class: "plot-label" }, tick.label));
    }
    svg.append(svgNode("path", { d: `M${left} ${topY} V${bottom} H${right}`, class: "plot-axis", fill: "none" }));
    plotted.forEach((row, index) => {
      let path = "", gap = true;
      for (const p of row.line.points || []) {
        if (!valid(p)) { gap = true; continue; }
        path += `${gap ? "M" : "L"}${x(p.time)} ${y(p.value)} `; gap = false;
        const dot = svgNode("circle", { cx: x(p.time), cy: y(p.value), r: 4, class: `plot-dot series-${index}` });
        dot.append(svgNode("title", {}, `${groupName(row.line.key)}: ${p.approximate ? "≈ " : ""}${format(of, Number(p.value))} · ${moment(p.time)}`)); svg.append(dot);
      }
      svg.append(svgNode("path", { d: path, fill: "none", class: `plot-line series-${index}` }));
    });
    chart.replaceChildren(svg);
  }

  if (typeof window !== "undefined" && window.addEventListener) window.addEventListener("resize", () => { if (state.active && !trend.hidden) render(); });
  tabs.metrics = { show() { state.active = true; updateRange(); load(); }, hide() { state.active = false; ++state.request; submit.disabled = false; } };
})();
