"use strict";
// Tools: every tool the MCP gateway serves (dev pod, factory, and other services), read-only.
// The sidebar is a tree: each MCP service is a folder, its tools are the rows inside it.
// Discovery is the same policy-filtered tools/list the Agents page offers in its tool picker.

(() => {
  const state = { services: [], errors: [], selected: "", seq: 0, loaded: false };
  const detail = $("tools-detail"), title = $("tools-detail-title");
  const enc = encodeURIComponent;
  const hashFor = (ref) => `#tools/${ref.split("/").map(enc).join("/")}`;
  const tree = createTree($("tools-list"), { onSelect: (node) => { if (location.hash !== hashFor(node.ref)) location.hash = hashFor(node.ref); } });
  const findTool = (ref) => state.services.flatMap((s) => s.tools).find((t) => t.ref === ref);
  const findService = (name) => state.services.find((s) => s.name === name);

  function renderTree() {
    tree.setRoots(state.services.map((s) => ({
      id: `s:${s.name}`, ref: s.name, label: s.name, title: `MCP service ${s.name}`,
      children: s.tools.map((t) => ({ id: `t:${t.ref}`, ref: t.ref, label: t.name, title: t.ref })),
    })));
    const notes = [];
    if (state.errors.length) notes.push(h("p", { className: "error-text", role: "alert" }, `Cannot list tools from ${state.errors.join(", ")}`));
    if (state.loaded && !state.services.length) notes.push(emptyPanel("No tools from the MCP services"));
    $("tools-empty").replaceChildren(...notes);
  }

  function renderDetail() {
    const tool = findTool(state.selected), service = findService(state.selected);
    title.textContent = tool ? tool.name : service ? service.name : "";
    if (tool) {
      detail.replaceChildren(
        h("section", { className: "section" }, h("h3", { className: "section-title" }, "Service"), h("p", {}, h("a", { href: hashFor(tool.service) }, tool.service))),
        h("section", { className: "section" }, h("h3", { className: "section-title" }, "Description"), h("p", {}, tool.description || "No description")),
        h("section", { className: "section" }, h("h3", { className: "section-title" }, "Input schema"),
          h("pre", { className: "code" }, JSON.stringify(tool.inputSchema || {}, null, 2))),
      );
    } else if (service) {
      detail.replaceChildren(h("section", { className: "section" }, h("h3", { className: "section-title" }, "Tools"),
        service.tools.length ? dataTable(["Name", "Description"], service.tools.map((t) =>
          h("tr", {}, h("td", {}, h("a", { href: hashFor(t.ref) }, t.name)), h("td", { className: "wrap-cell" }, t.description || "No description"))))
          : h("p", {}, "This service offers no tools")));
    } else detail.replaceChildren(emptyPanel("No tool selected"));
  }

  // select shows a tool ("service/tool") or a service ("service"); anything else shows the first service.
  async function select(ref) {
    const tool = findTool(ref);
    state.selected = tool || findService(ref) ? ref : state.services[0]?.name || "";
    renderTree();
    if (tool) { await tree.reveal([`s:${tool.service}`]); await tree.select(`t:${ref}`); }
    else await tree.select(state.selected ? `s:${state.selected}` : null);
    renderDetail();
    if (state.selected && ref !== state.selected) location.hash = hashFor(state.selected);
  }

  async function load(ref) {
    const seq = ++state.seq;
    let services = [], errors = [];
    try {
      const response = await api("GET", "/api/mcp-gateway/api/services");
      const listed = (response.services || []).map((s) => typeof s === "string" ? { name: s } : s).filter((s) => s.name);
      errors = listed.filter((s) => s.error).map((s) => s.name);
      const ok = listed.filter((s) => !s.error);
      const results = await Promise.allSettled(ok.map(async (service) => {
        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), 10000);
        try { return (await window.discoverMcpServiceTools(service.name, controller.signal)).map((t) => ({
          ref: `${service.name}/${t.name}`, service: service.name, name: t.name, description: t.description || "", inputSchema: t.inputSchema,
        })).sort((a, b) => a.name.localeCompare(b.name)); } finally { clearTimeout(timeout); }
      }));
      results.forEach((r, i) => {
        if (r.status === "fulfilled") services.push({ name: ok[i].name, tools: r.value });
        else errors.push(ok[i].name);
      });
    } catch { errors = ["the MCP gateway"]; }
    if (seq !== state.seq || activeTab !== "tools") return;
    state.services = services.sort((a, b) => a.name.localeCompare(b.name));
    state.errors = errors;
    state.loaded = true;
    await select(ref);
  }

  tabs.tools = {
    show(rest) {
      let ref = rest.replace(/^\/+/, "").split(/[?]/)[0];
      try { ref = ref.split("/").map(decodeURIComponent).join("/"); } catch { /* an unknown ref shows the first service */ }
      return state.loaded ? select(ref) : load(ref);
    },
    hide() { state.seq++; state.loaded = false; },
  };
})();
