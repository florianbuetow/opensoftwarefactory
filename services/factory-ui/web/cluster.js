"use strict";
// Cluster tab: the factory namespace, polled every 5 seconds while visible. The sidebar
// tree holds deployments (with their pods), services, and nodes; the content shows the
// selected item: a deployment's pods, service, and events; a pod's events and logs; a
// service's ports and pods; a node's state. The first deployment is selected by default.
// Hash: #cluster/<kind>:<name>.

(() => {
  const API = "/api/cluster";
  const state = { data: null, error: "", selected: "", poll: null, follow: null, seq: 0, opened: false, logs: null };
  const body = $("cluster-detail");
  const tree = createTree($("cluster-tree"), { onSelect(n) { if (n.kind) location.hash = `#cluster/${n.id}`; } });

  const sortByName = (items) => [...items].sort((a, b) => a.metadata.name.localeCompare(b.metadata.name));

  // podStatus mirrors kubectl's STATUS column: a waiting or terminated reason wins over the phase.
  function podStatus(p) {
    if (p.metadata.deletionTimestamp) return "Terminating";
    for (const c of [...(p.status?.initContainerStatuses || []), ...(p.status?.containerStatuses || [])]) {
      const reason = c.state?.waiting?.reason || (c.state?.terminated && c.state.terminated.reason !== "Completed" ? c.state.terminated.reason : "");
      if (reason) return reason;
    }
    return p.status?.reason || p.status?.phase || "Unknown";
  }
  const podReady = (p) => `${(p.status?.containerStatuses || []).filter((c) => c.ready).length}/${(p.spec?.containers || []).length}`;
  const restarts = (p) => (p.status?.containerStatuses || []).reduce((n, c) => n + (c.restartCount || 0), 0);
  const healthy = (status) => ["Running", "Succeeded", "Completed"].includes(status);
  const nodeReady = (n) => (n.status?.conditions || []).find((c) => c.type === "Ready")?.status === "True";
  const matches = (selector, p) => { const e = Object.entries(selector || {}); return e.length > 0 && e.every(([k, v]) => p.metadata.labels?.[k] === v); };
  const podsOf = (selector) => sortByName(state.data.pods).filter((p) => matches(selector, p));
  const deploymentOf = (pod) => state.data.deployments.find((d) => matches(d.spec?.selector?.matchLabels, pod));
  const suffix = (pod) => { const d = deploymentOf(pod); return d && pod.metadata.name.startsWith(`${d.metadata.name}-`) ? pod.metadata.name.slice(d.metadata.name.length + 1) : pod.metadata.name; };
  // A Kubernetes status shows like a run state, a dot plus the word: green when healthy,
  // amber while starting or stopping, red otherwise.
  const statusText = (text, ok) => statusChip(ok ? "done" : /^(Pending|ContainerCreating|PodInitializing|Terminating)$/.test(text) ? "running" : "failed", text);
  const when = (e) => e.lastTimestamp || e.eventTime || e.firstTimestamp || e.metadata.creationTimestamp;

  async function fetchText(path) {
    const res = await fetch(path);
    const text = await res.text();
    if (!res.ok) throw new Error(errorMessage(text, res));
    return text;
  }

  async function refresh() {
    if (document.hidden) return;
    const seq = ++state.seq;
    try {
      const [nodes, deployments, pods, services, events] = await Promise.all(
        ["nodes", "deployments", "pods", "services", "events"].map((r) => api("GET", `${API}/${r}`)));
      if (seq !== state.seq) return;
      state.data = { nodes: nodes.items, deployments: deployments.items, pods: pods.items, services: services.items, events: events.items };
      state.error = "";
    } catch (e) {
      if (seq !== state.seq) return;
      state.error = /not running in a cluster/i.test(e.message) ? "Not running in a cluster" : `Cannot read the cluster: ${e.message}`;
    }
    renderTree();
    if (!state.revealed && state.data) { state.revealed = true; revealSelected(); }
    renderDetail(false);
  }

  // revealSelected opens the tree down to the selected item and marks it.
  function revealSelected() {
    const id = state.selected;
    if (!id.includes(":")) return;
    const [kind, ...r] = id.split(":");
    let path = [{ deployment: "deployments", service: "services", node: "nodes" }[kind]];
    if (kind === "pod") {
      const pod = state.data?.pods.find((p) => p.metadata.name === r.join(":"));
      const parent = pod && deploymentOf(pod);
      path = parent ? ["deployments", `deployment:${parent.metadata.name}`] : ["pods"];
    }
    tree.reveal(path.filter(Boolean)).then(() => tree.select(id));
  }

  // ---- Tree: a status is shown only where something is wrong ----

  function renderTree() {
    const d = state.data;
    if (!d) { tree.setRoots([]); return; }
    const podNode = (p) => {
      const status = podStatus(p);
      return { id: `pod:${p.metadata.name}`, kind: "pod", label: suffix(p), title: p.metadata.name, sub: healthy(status) ? "" : status, className: healthy(status) ? "" : "warn" };
    };
    const owned = new Set();
    const deployments = sortByName(d.deployments).map((x) => {
      const pods = podsOf(x.spec?.selector?.matchLabels);
      pods.forEach((p) => owned.add(p.metadata.name));
      const desired = x.spec?.replicas ?? 0, ready = x.status?.readyReplicas || 0;
      // A deployment short of replicas says so in a word, the counts in its tooltip: never a bare "0/1".
      const short = ready < desired;
      return { id: `deployment:${x.metadata.name}`, kind: "deployment", label: x.metadata.name, title: short ? `${x.metadata.name}: ${ready} of ${desired} replicas ready` : x.metadata.name, sub: short ? (ready ? "Starting" : "Not ready") : "", className: short ? "warn" : "", children: pods.map(podNode) };
    });
    const others = sortByName(d.pods).filter((p) => !owned.has(p.metadata.name)).map(podNode);
    const roots = [{ id: "deployments", label: "Deployments", children: deployments }];
    if (others.length) roots.push({ id: "pods", label: "Other pods", children: others });
    roots.push(
      { id: "services", label: "Services", children: sortByName(d.services).map((x) => ({ id: `service:${x.metadata.name}`, kind: "service", label: x.metadata.name })) },
      { id: "nodes", label: d.nodes.length === 1 ? "Node" : "Nodes", children: d.nodes.map((n) => ({ id: `node:${n.metadata.name}`, kind: "node", label: n.metadata.name, full: n.metadata.name, sub: nodeReady(n) ? "" : "Not ready", className: nodeReady(n) ? "" : "warn" })) },
    );
    tree.setRoots(roots);
    if (!state.opened) { state.opened = true; tree.reveal(["deployments"]); }
  }

  // ---- Detail of the selected item ----

  const section = (title, ...content) => h("section", { className: "section" }, h("h3", { className: "section-title" }, title), ...content);
  const metaLine = (...parts) => h("div", { className: "run-meta" }, ...parts.filter(Boolean).flatMap((p, i) => (i ? [h("span", { className: "sep" }, "·"), p] : [p])));

  function webUILink(s) {
    const url = s.metadata.annotations?.["opensoftwarefactory.dev/url"];
    if (!url) return null;
    return /^https?:\/\//i.test(url) ? h("a", { href: url, target: "_blank", rel: "noopener" }, url) : h("span", {}, url);
  }
  const ports = (s) => (s.spec?.ports || []).map((p) => `${p.port}${p.nodePort ? `:${p.nodePort}` : ""}/${p.protocol || "TCP"}`).join(", ");

  function podTable(pods) {
    return dataTable(["Pod", "Status", "Ready", "Restarts"], pods.map((p) => {
      const status = podStatus(p), n = restarts(p);
      return clickableRow(h("tr", {},
        h("td", { className: "name-cell", title: p.metadata.name }, suffix(p)),
        h("td", {}, statusText(status, healthy(status))),
        h("td", { className: "num" }, podReady(p)),
        h("td", { className: "num" }, n ? h("span", { className: "error-text" }, String(n)) : "0")),
      () => { location.hash = `#cluster/pod:${p.metadata.name}`; });
    }), { numeric: [2, 3], name: 0 });
  }

  function eventsSection(names) {
    const events = state.data.events.filter((e) => names.includes(e.involvedObject?.name)).sort((a, b) => new Date(when(b)) - new Date(when(a))).slice(0, 20);
    if (!events.length) return null;
    return section("Events", dataTable(["When", "Reason", "Message"], events.map((e) => h("tr", {},
      h("td", {}, timeEl(when(e)), e.count > 1 ? ` ×${e.count}` : ""),
      h("td", { className: e.type === "Warning" ? "error-text" : "" }, e.reason || ""),
      h("td", { className: "wrap-cell" }, e.message || "")))));
  }

  // renderDetail redraws the selected item; a pod's log view is kept while polling.
  function renderDetail(reset = true) {
    const d = state.data;
    if (state.error && !d) { body.replaceChildren(errorPanel(state.error)); return; }
    if (!d) return;
    if (!state.selected) {
      const first = sortByName(d.deployments)[0];
      if (first) { location.replace(`#cluster/deployment:${first.metadata.name}`); return; }
    }
    const [kind, ...rest] = state.selected.split(":");
    const name = rest.join(":");
    // A selected item that is gone gives way, never a ghost: a pod (renamed on every restart) to
    // its deployment, anything else to the first deployment.
    const items = { pod: d.pods, deployment: d.deployments, service: d.services, node: d.nodes }[kind];
    if (items && !items.some((x) => x.metadata.name === name)) {
      const owner = kind === "pod" ? d.deployments.filter((x) => name.startsWith(`${x.metadata.name}-`)).sort((a, b) => b.metadata.name.length - a.metadata.name.length)[0] : null;
      location.replace(owner ? `#cluster/deployment:${owner.metadata.name}` : "#cluster");
      return;
    }
    if (kind === "pod") { renderPod(name, reset); return; }
    stopLogs();
    if (kind === "deployment") {
      const x = d.deployments.find((y) => y.metadata.name === name);
      if (!x) { body.replaceChildren(); return; }
      const pods = podsOf(x.spec?.selector?.matchLabels);
      const services = d.services.filter((s) => pods.some((p) => matches(s.spec?.selector, p)));
      body.replaceChildren(...[
        pods.length ? section("Pods", podTable(pods)) : null,
        // The same line as the service view; the name only when it differs from the deployment.
        ...services.map((s) => section("Service", metaLine(s.metadata.name !== name ? h("span", {}, s.metadata.name) : null, h("span", {}, s.spec?.type || ""), h("span", {}, s.spec?.clusterIP || ""), h("span", {}, ports(s)), webUILink(s)))),
        eventsSection([name, ...pods.map((p) => p.metadata.name)])].filter(Boolean));
    } else if (kind === "service") {
      const s = d.services.find((y) => y.metadata.name === name);
      if (!s) { body.replaceChildren(); return; }
      const pods = podsOf(s.spec?.selector);
      body.replaceChildren(...[
        h("div", { className: "section" }, metaLine(h("span", {}, s.spec?.type || ""), h("span", {}, s.spec?.clusterIP || ""), h("span", {}, ports(s)), webUILink(s))),
        pods.length ? section("Pods", podTable(pods)) : null].filter(Boolean));
    } else if (kind === "node") {
      const n = d.nodes.find((y) => y.metadata.name === name);
      if (!n) { body.replaceChildren(); return; }
      body.replaceChildren(h("div", { className: "section" }, metaLine(
        statusText(nodeReady(n) ? "Ready" : "NotReady", nodeReady(n)),
        h("span", {}, n.status?.nodeInfo?.kubeletVersion || ""),
        h("span", {}, n.status?.nodeInfo?.osImage || ""))));
    }
  }

  // ---- Pod: events, then the logs, which grow with the panel ----

  function renderPod(name, reset) {
    const pod = state.data.pods.find((p) => p.metadata.name === name);
    if (state.logs?.pod === name && !reset) {
      const ev = eventsSection([name]);
      state.logs.events.replaceChildren(...(ev ? [ev] : []));
      return;
    }
    stopLogs();
    const containers = [...(pod?.spec?.initContainers || []), ...(pod?.spec?.containers || [])].map((c) => c.name);
    const main = (pod?.spec?.containers || [])[0]?.name;
    const c = {
      pod: name,
      container: h("select", { "aria-label": "Container", className: "fit" }, ...containers.map((x) => h("option", { value: x, selected: x === main }, x))),
      tail: h("select", { "aria-label": "Log lines", className: "fit" }, ...[100, 500, 2000].map((n) => h("option", { value: n }, `Last ${n} lines`))),
      previous: h("input", { type: "checkbox" }),
      follow: h("input", { type: "checkbox" }),
      wrap: h("input", { type: "checkbox", checked: true }),
      text: h("pre", { className: "file-block code logs wrap", tabIndex: 0, "aria-label": `Logs of ${name}` }),
      events: h("div"),
    };
    state.logs = c;
    for (const x of [c.container, c.tail, c.previous]) x.onchange = () => loadLogs();
    c.follow.onchange = () => setFollow(c.follow.checked);
    c.wrap.onchange = () => c.text.classList.toggle("wrap", c.wrap.checked);
    const ev = eventsSection([name]);
    c.events.replaceChildren(...(ev ? [ev] : []));
    body.replaceChildren(
      c.events,
      h("section", { className: "section" },
        h("div", { className: "row log-controls" },
          containers.length > 1 ? c.container : null,
          c.tail,
          h("label", { className: "check", title: "Shows the logs of the previous container instance, after a restart" }, c.previous, " Previous"),
          h("label", { className: "check", title: "Reloads every 3 seconds and stays at the end" }, c.follow, " Follow"),
          h("label", { className: "check" }, c.wrap, " Wrap lines"),
          h("span", { className: "spacer" }),
          copyButton(() => c.text.textContent || "", "logs")),
        c.text));
    loadLogs(true);
  }

  async function loadLogs(toEnd = false) {
    const c = state.logs;
    if (!c) return;
    const q = new URLSearchParams({ tailLines: c.tail.value });
    if (c.container.value) q.set("container", c.container.value);
    if (c.previous.checked) q.set("previous", "true");
    try {
      const text = await fetchText(`${API}/pods/${encodeURIComponent(c.pod)}/log?${q}`);
      if (c !== state.logs) return;
      c.text.textContent = pretty(text);
      c.text.classList.remove("error-text");
    } catch (e) {
      if (c !== state.logs) return;
      c.text.textContent = e.message;
      c.text.classList.add("error-text");
    }
    if (toEnd || c.follow.checked) body.scrollTop = body.scrollHeight;
  }

  function setFollow(on) {
    clearInterval(state.follow);
    state.follow = on ? setInterval(() => loadLogs(true), 3000) : null;
  }
  function stopLogs() { setFollow(false); state.logs = null; }

  tabs.cluster = {
    show(rest) {
      const id = decodeURIComponent(rest.replace(/^\//, ""));
      const changed = id !== state.selected;
      state.selected = id;
      if (state.data) revealSelected();
      if (changed) renderDetail(true);
      if (state.poll) return;
      refresh();
      state.poll = setInterval(refresh, 5000);
    },
    hide() {
      clearInterval(state.poll);
      state.poll = null;
      setFollow(false);
    },
  };
})();
