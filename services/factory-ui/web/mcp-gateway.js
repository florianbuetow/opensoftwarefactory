"use strict";
// MCP Gateway configuration and quick testing. MCP servers remain external; this page edits
// gateway passthrough configuration and sends test requests through the gateway itself.

(() => {
  const API = "/api/mcp-gateway/api/services";
  const MCP = "/api/mcp-gateway/mcp";
  const VERSION = "2025-11-25";
  const DISCOVERY_TIMEOUT = 5000;
  const state = { services: [], selected: "", config: null, seq: 0, request: 0, tools: [], discovery: null, session: null, call: null, editor: null, testValues: null };
  const list = $("mcp-services"), empty = $("mcp-empty"), config = $("mcp-config"), test = $("mcp-test");

  const enc = encodeURIComponent;
  const selectedHash = (name) => `#mcp-gateway/${enc(name)}`;
  const validName = (name) => /^[A-Za-z0-9_-][A-Za-z0-9._-]*$/.test(name);
  const asService = (item) => typeof item === "string" ? { name: item } : item;
  const pageIsActive = () => typeof activeTab === "undefined" || activeTab === "mcp-gateway";
  const current = (seq, name = state.selected) => seq === state.seq && name === state.selected && pageIsActive();

  function hasEdits() {
    const editor = state.editor;
    return editor?.name === state.selected && Object.keys(editor.areas).some((key) => editor.areas[key].value !== editor.original[key]);
  }

  function newService() {
    const input = h("input", { id: "mcp-new-name", autocomplete: "off", autofocus: true });
    const problem = h("p", { className: "error-text", role: "alert" });
    const create = h("button", { type: "button", className: "primary", onclick: async () => {
      const name = input.value.trim();
      if (!validName(name)) { problem.textContent = "Use one Blobstore name segment."; input.focus(); return; }
      if (state.services.some((s) => s.name === name)) { problem.textContent = `${name} already exists.`; input.focus(); return; }
      const seq = state.seq;
      create.disabled = true;
      try {
        await api("PUT", `${API}/${enc(name)}`, {
          mcpconfig: { transport: "streamable-http", url: "https://example.invalid/mcp" },
          allowlist: [], denylist: [],
        });
        if (!current(seq)) return;
        dlg.close();
        setSaveState("Created");
        await refresh(name);
      } catch (e) {
        if (current(seq)) setStatus(`Not saved: ${e.message}`, "error");
      } finally { create.disabled = false; }
    } }, "Create");
    const cancel = h("button", { type: "button", onclick: () => dlg.close() }, "Cancel");
    const dlg = openDialog({
      title: "New MCP service",
      body: h("div", { className: "field" }, h("label", { htmlFor: input.id }, "Name"), input, problem),
      footer: h("div", { className: "row" }, cancel, create),
    });
  }

  async function deleteService(name) {
    if (!(await confirmAction(`Delete ${name}?`, "Delete"))) return;
    const at = state.services.findIndex((s) => s.name === name);
    const neighbour = state.services[at + 1] || state.services[at - 1];
    const seq = state.seq, selected = state.selected;
    const closesSelected = selected === name;
    try {
      if (closesSelected) {
        state.tools = [];
        renderTest();
        await resetConnection();
      }
      await api("DELETE", `${API}/${enc(name)}`);
      if (!current(seq, selected)) return;
      setSaveState("Deleted");
      if (state.selected === name) { state.editor = null; await refresh(neighbour?.name || ""); }
      else await refresh(state.selected);
    } catch (e) {
      if (!current(seq, selected)) return;
      setStatus(`Not deleted: ${e.message}`, "error");
      if (closesSelected) void loadTools(selected, seq, false);
    }
  }

  function renderList() {
    list.replaceChildren(...state.services.map((s) => {
      const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(s.name === state.selected) },
        h("span", { className: "row-title", dataset: { full: s.name } }, s.name));
      const choose = () => { if (s.name !== state.selected) location.hash = selectedHash(s.name); };
      li.addEventListener("click", choose);
      li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); choose(); } });
      li.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["Delete service", () => deleteService(s.name), "danger", `Deletes ${s.name}`],
        "-",
        ["New MCP service…", newService, "", "Creates an MCP passthrough service"],
      ]));
      return li;
    }));
    empty.replaceChildren(...(state.services.length ? [] : [emptyPanel("No MCP services yet")]));
  }

  const editor = (id, label, value) => {
    const area = h("textarea", { id, className: "mcp-editor code", spellcheck: false, value: JSON.stringify(value, null, 2) });
    return { area, section: h("section", { className: "section" }, h("label", { htmlFor: id, className: "section-title" }, label), area) };
  };

  function parse(area, label) {
    try { return JSON.parse(area.value); }
    catch (e) { area.focus(); throw new Error(`${label}: ${e.message}`); }
  }

  function renderConfig() {
    $("mcp-config-title").textContent = state.selected;
    if (!state.selected || !state.config) { state.editor = null; config.replaceChildren(); return; }
    const mcpconfig = editor("mcpconfig-json", "mcpconfig.json", state.config.mcpconfig);
    const allowlist = editor("mcp-allowlist-json", "allowlist.json", state.config.allowlist || []);
    const denylist = editor("mcp-denylist-json", "denylist.json", state.config.denylist || []);
    const areas = { mcpconfig: mcpconfig.area, allowlist: allowlist.area, denylist: denylist.area };
    const editorState = { name: state.selected, areas, original: Object.fromEntries(Object.entries(areas).map(([key, area]) => [key, area.value])) };
    state.editor = editorState;
    const problem = h("p", { className: "error-text", role: "alert" });
    const save = h("button", { type: "button", className: "primary", onclick: async () => {
      problem.textContent = "";
      const name = state.selected, seq = state.seq;
      const submitted = Object.fromEntries(Object.entries(areas).map(([key, area]) => [key, area.value]));
      let body;
      try {
        body = {
          mcpconfig: parse(mcpconfig.area, "mcpconfig.json"),
          allowlist: parse(allowlist.area, "allowlist.json"),
          denylist: parse(denylist.area, "denylist.json"),
        };
      } catch (e) { problem.textContent = e.message; return; }
      save.disabled = true;
      try {
        state.tools = [];
        renderTest();
        await resetConnection();
        await api("PUT", `${API}/${enc(name)}`, body);
        if (name !== state.selected || state.editor !== editorState) return;
        state.config = body;
        editorState.original = submitted;
        if (seq === state.seq) { setSaveState(); void loadTools(name, seq, false); }
      } catch (e) {
        if (current(seq, name)) {
          setStatus(`Not saved: ${e.message}`, "error");
          void loadTools(name, seq, false);
        }
      }
      finally { save.disabled = false; }
    } }, "Save");
    config.replaceChildren(mcpconfig.section, allowlist.section, denylist.section,
      h("section", { className: "section actions" }, problem, save));
  }

  const requestHeaders = (session = "") => ({
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
    "MCP-Protocol-Version": VERSION,
    ...(session ? { "Mcp-Session-Id": session } : {}),
  });

  async function deleteSession(session) {
    if (!session?.id) return;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 2000);
    try {
      await fetch(`${MCP}/${enc(session.service)}`, {
        method: "DELETE", headers: requestHeaders(session.id), signal: controller.signal,
      });
    } catch { /* session deletion is optional and best effort */ }
    finally { clearTimeout(timeout); }
  }

  async function closeSession() {
    const session = state.session;
    state.session = null;
    await deleteSession(session);
  }

  async function cancelToolCall() {
    const call = state.call;
    state.call = null;
    if (!call) return;
    call.controller.abort();
    if (call.id === null) return;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 2000);
    try {
      await mcpNotification(call.service, "notifications/cancelled", {
        requestId: call.id, reason: "Quick test stopped",
      }, call.session, controller.signal);
    } catch { /* cancellation is best effort before session cleanup */ }
    finally { clearTimeout(timeout); }
  }

  async function resetConnection() {
    state.discovery?.abort();
    await cancelToolCall();
    await closeSession();
  }

  async function sseResponse(body, id) {
    if (!body) throw new Error("MCP response has no body");
    const reader = body.getReader(), decoder = new TextDecoder();
    let buffer = "", data = [];
    const event = () => {
      if (!data.length) return null;
      const raw = data.join("\n");
      data = [];
      try { const message = JSON.parse(raw); return message.id === id ? message : null; }
      catch { return null; }
    };
    while (true) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
      let newline;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        let line = buffer.slice(0, newline);
        buffer = buffer.slice(newline + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        if (!line) {
          const message = event();
          if (message) { await reader.cancel(); return message; }
        } else if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
      }
      if (done) {
        if (buffer.startsWith("data:")) data.push(buffer.slice(5).trimStart());
        const message = event();
        if (message) return message;
        break;
      }
    }
    throw new Error("MCP response did not contain the request result");
  }

  async function mcpCall(service, method, params = {}, session = "", signal, onRequest) {
    const id = ++state.request;
    onRequest?.(id);
    const res = await fetch(`${MCP}/${enc(service)}`, {
      method: "POST", headers: requestHeaders(session),
      signal,
      body: JSON.stringify({ jsonrpc: "2.0", id, method, params }),
    });
    const responseSession = res.headers.get("Mcp-Session-Id") || session;
    const type = (res.headers.get("Content-Type") || "").split(";")[0];
    if (!res.ok) {
      const text = await res.text();
      let response; try { response = JSON.parse(text); } catch { /* non-JSON upstream error */ }
      const error = new Error(response?.error?.message || (typeof response?.error === "string" ? response.error : text) || `HTTP ${res.status}`);
      if (response) error.rpc = response;
      error.session = responseSession;
      throw error;
    }
    let response;
    try { response = type === "text/event-stream" ? await sseResponse(res.body, id) : JSON.parse(await res.text()); }
    catch (error) { error.session = responseSession; throw error; }
    if (response.error) {
      const error = new Error(response.error.message || "MCP request failed");
      error.rpc = response;
      error.session = responseSession;
      throw error;
    }
    return { response, session: responseSession };
  }

  async function mcpNotification(service, method, params, session, signal) {
    const res = await fetch(`${MCP}/${enc(service)}`, {
      method: "POST", headers: requestHeaders(session), signal,
      body: JSON.stringify({ jsonrpc: "2.0", method, ...(params === undefined ? {} : { params }) }),
    });
    if (!res.ok) { const text = await res.text(); throw new Error(errorMessage(text, res)); }
  }

  async function initializeSession(service, signal) {
    let response, session;
    try {
      ({ response, session } = await mcpCall(service, "initialize", {
        protocolVersion: VERSION,
        capabilities: {},
        clientInfo: { name: "factory-ui", version: "1" },
      }, "", signal));
    } catch (error) {
      if (error.session) await deleteSession({ service, id: error.session });
      throw error;
    }
    const result = response.result;
    if (result?.protocolVersion !== VERSION) {
      await deleteSession({ service, id: session });
      throw new Error(`MCP server selected unsupported protocol ${result?.protocolVersion || "version"}`);
    }
    if (!result.capabilities || typeof result.capabilities.tools !== "object" || result.capabilities.tools === null) {
      await deleteSession({ service, id: session });
      throw new Error("MCP server does not advertise tools");
    }
    try {
      await mcpNotification(service, "notifications/initialized", undefined, session, signal);
      return { service, id: session };
    } catch (error) {
      await deleteSession({ service, id: session });
      throw error;
    }
  }

  async function discoverTools(service, session, signal) {
    const tools = [];
    let cursor = "";
    do {
      const { response } = await mcpCall(service, "tools/list", cursor ? { cursor } : {}, session, signal);
      tools.push(...(response.result?.tools || []));
      cursor = response.result?.nextCursor || "";
    } while (cursor);
    return tools;
  }

  // The Agents page uses the same policy-filtered discovery as this page's quick test.
  window.discoverMcpServiceTools = async (service, signal) => {
    const session = await initializeSession(service, signal);
    try { return await discoverTools(service, session.id, signal); }
    finally { await deleteSession(session); }
  };

  async function loadTools(service, seq, report = true) {
    if (!service) return;
    await resetConnection();
    const controller = new AbortController();
    state.discovery = controller;
    let timedOut = false;
    const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, DISCOVERY_TIMEOUT);
    let initialized = null;
    try {
      initialized = await initializeSession(service, controller.signal);
      if (seq !== state.seq || service !== state.selected) { await deleteSession(initialized); return; }
      state.session = initialized;
      const tools = await discoverTools(service, initialized.id, controller.signal);
      if (seq !== state.seq || service !== state.selected) {
        if (state.session === initialized) await closeSession();
        else await deleteSession(initialized);
        return;
      }
      state.tools = tools;
      renderTest();
      if (report) setSaveState("Connected");
    } catch (e) {
      if (state.session === initialized) await closeSession();
      else if (initialized) await deleteSession(initialized);
      if (seq !== state.seq || service !== state.selected || (controller.signal.aborted && !timedOut)) return;
      state.tools = [];
      const message = timedOut ? "Discovery timed out" : e.message;
      renderTest(message);
      if (report) setStatus(`Cannot connect: ${message}`, "error");
    } finally {
      clearTimeout(timeout);
      if (state.discovery === controller) state.discovery = null;
    }
  }

  function renderTest(loadError = "") {
    if (!state.selected) { test.replaceChildren(); return; }
    if (state.testValues?.service !== state.selected) state.testValues = { service: state.selected, tool: "", args: "{}", result: "" };
    const values = state.testValues;
    if (loadError) values.result = JSON.stringify({ error: loadError }, null, 2);
    const connection = h("button", { type: "button", disabled: !!state.call, title: state.call ? "Tool call in progress" : "Checks connection and tool discovery", onclick: async () => {
      const service = state.selected, seq = state.seq;
      connection.disabled = true;
      send.disabled = true;
      try {
        await api("POST", `${API}/${enc(service)}/test-connection`);
        if (seq !== state.seq || service !== state.selected) return;
        await loadTools(service, seq, true);
      } catch (e) {
        if (seq !== state.seq || service !== state.selected) return;
        setStatus(`Cannot connect: ${e.message}`, "error");
      } finally { connection.disabled = false; send.disabled = !state.tools.length || !!state.call || state.session?.service !== state.selected; }
    } }, "Test connection");
    const select = h("select", { id: "mcp-test-tool" },
      state.tools.length ? null : h("option", { value: "" }, "No allowed tools"),
      ...state.tools.map((tool) => h("option", { value: tool.name }, tool.name)));
    if (state.tools.length) {
      if (!state.tools.some((tool) => tool.name === values.tool)) values.tool = state.tools[0].name;
      select.value = values.tool;
    }
    select.addEventListener("change", () => { values.tool = select.value; });
    const args = h("textarea", { id: "mcp-test-arguments", className: "code", spellcheck: false, value: values.args });
    args.addEventListener("input", () => { values.args = args.value; });
    const result = h("pre", { className: "file-block code mcp-result" }, values.result);
    const send = h("button", { type: "button", disabled: !state.tools.length || !!state.call || state.session?.service !== state.selected, title: state.tools.length ? "Invokes the selected tool through the gateway" : "Needs an allowed tool", onclick: async () => {
      const service = state.selected, seq = state.seq;
      values.args = args.value; values.tool = select.value;
      send.disabled = true;
      const controller = new AbortController();
      const active = { service, session: state.session?.id || "", id: null, controller };
      state.call = active;
      connection.disabled = true;
      try {
        const value = parse(args, "Arguments");
        if (!value || Array.isArray(value) || typeof value !== "object") throw new Error("Arguments must be a JSON object");
        const session = state.session?.service === service ? state.session.id : "";
        if (!state.session || state.session.service !== service) throw new Error("MCP session is not initialized");
        const { response } = await mcpCall(service, "tools/call", { name: select.value, arguments: value }, session, controller.signal, (id) => { active.id = id; });
        if (seq !== state.seq || service !== state.selected) return;
        values.result = JSON.stringify(response, null, 2);
        result.textContent = values.result;
      } catch (e) {
        if (controller.signal.aborted) return;
        if (seq !== state.seq || service !== state.selected) return;
        values.result = JSON.stringify(e.rpc || { error: e.message }, null, 2);
        result.textContent = values.result;
      } finally {
        if (state.call === active) state.call = null;
        send.disabled = !state.tools.length || !!state.call;
        connection.disabled = !!state.call;
      }
    } }, "Send");
    test.replaceChildren(
      h("section", { className: "section actions" }, connection),
      h("section", { className: "section" },
        h("div", { className: "field" }, h("label", { htmlFor: select.id }, "Tool"), select),
        h("div", { className: "field" }, h("label", { htmlFor: args.id }, "Arguments"), args),
        h("div", { className: "mcp-send" }, send),
        h("div", { className: "field" }, h("label", { className: "section-title" }, "Result"), result)));
  }

  async function load(name) {
    if (name !== state.selected && hasEdits()) {
      if (!(await confirmAction("Discard your unsaved changes?", "Discard"))) {
        history.replaceState(null, "", selectedHash(state.selected));
        return;
      }
    }
    const keepEditor = name === state.selected && hasEdits();
    const seq = ++state.seq;
    await resetConnection();
    if (seq !== state.seq) return;
    if (keepEditor) {
      renderList();
      void loadTools(name, seq, false);
      return;
    }
    state.selected = name;
    state.config = null;
    state.tools = [];
    renderList(); renderConfig(); renderTest();
    try {
      const loaded = await api("GET", `${API}/${enc(name)}`);
      if (seq !== state.seq) return;
      state.config = loaded;
      renderConfig();
      void loadTools(name, seq, false);
    } catch (e) {
      if (seq !== state.seq) return;
      config.replaceChildren(errorPanel(`Cannot load ${name}: ${e.message}`, () => load(name)));
    }
  }

  async function refresh(preferred = state.selected) {
    const seq = state.seq, selected = state.selected;
    try {
      const response = await api("GET", API);
      if (!current(seq, selected)) return;
      state.services = (response.services || []).map(asService).filter((s) => s.name).sort((a, b) => a.name.localeCompare(b.name));
      const chosen = state.services.find((s) => s.name === preferred)?.name || state.services[0]?.name || "";
      if (!chosen) {
        state.selected = ""; state.config = null; state.tools = [];
        renderList(); renderConfig(); renderTest();
      } else if (location.hash !== selectedHash(chosen)) location.hash = selectedHash(chosen);
      else await load(chosen);
    } catch (e) {
      if (!current(seq, selected)) return;
      list.replaceChildren();
      empty.replaceChildren(errorPanel(`Cannot list MCP services: ${e.message}`, () => refresh(preferred)));
      config.replaceChildren(); test.replaceChildren();
    }
  }

  $("mcp-new-slot").append(iconButton("plus", "New MCP service", newService));
  $("mcp-sidebar").addEventListener("contextmenu", (ev) => {
    if (!ev.target.closest("li")) openMenu(ev, [["New MCP service…", newService, "", "Creates an MCP passthrough service"]]);
  });
  window.addEventListener("beforeunload", (ev) => { if (hasEdits()) ev.preventDefault(); });
  tabs["mcp-gateway"] = {
    show(rest) {
      let name = rest.replace(/^\/+/, "").split(/[?]/)[0];
      try { name = decodeURIComponent(name); } catch { /* gateway validation reports invalid names */ }
      if (!state.services.length) refresh(name);
      else if (name && state.services.some((s) => s.name === name)) load(name);
      else refresh(name);
    },
    hide() {
      state.seq++;
      void resetConnection();
    },
  };
})();
