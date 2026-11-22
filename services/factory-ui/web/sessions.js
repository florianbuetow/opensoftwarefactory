"use strict";
// Sessions page: the interactive sessions waiting for a person in the sidebar (polled every
// 3 seconds, like the gates on Decisions), the chat and file tabs of the selected one in the
// middle, and its files on the right (see chat.js). A session that no longer waits, opened
// from a finished run, is shown read-only and stays in the list while the page is open.
// Hash: #sessions/<trace>/<step>, the session's blobstore path without user-sessions/.

(() => {
  const POLL_MS = 3000;
  const LAST_KEY = "factory.sessions.last";
  const state = { wanted: "", open: "", controller: null, timer: null, listKey: null, opening: false };
  const kept = new Map(); // session -> entry: sessions opened here, listed also once they stop waiting
  const probed = new Map(); // session -> whether the blobstore holds it (asked once per linked session)
  const sidebar = $("session-list");
  const listState = $("session-list-state");
  const main = $("session-main");
  const files = $("session-files");

  const started = (e) => runIndex.records.get(e.trace_id)?.started || "";

  // entries lists the waiting sessions, newest run first, then the ones kept from this visit.
  function entries() {
    const waiting = sessionsState.pending.filter((p) => ChatLogic.validSession(p.session)).map((p) => ({ ...p, waiting: true }))
      .sort((a, b) => String(started(b)).localeCompare(String(started(a))));
    const live = new Set(waiting.map((e) => e.session));
    return [...waiting, ...[...kept.values()].filter((e) => !live.has(e.session)).map((e) => ({ ...e, waiting: false }))];
  }

  // A session is named by its workflow and step; a run without a known workflow by its trace ID.
  const label = (e) => `${titleCase(e.workflow || runIndex.records.get(e.trace_id)?.workflow) || midTruncate(e.trace_id || "", 13)} › ${stepTitle(e.step, e.pass || stepPass(e.session || "").pass)}`; // with its pass, for a step that runs in passes

  function renderList() {
    const list = entries();
    const problem = !sessionsState.loaded && sessionsState.error ? `Cannot load sessions: ${sessionsState.error}` : "";
    const key = JSON.stringify([list.map((e) => [e.session, label(e), started(e), formatTime(started(e))]), state.open, problem, sessionsState.loaded]);
    if (key === state.listKey) return;
    state.listKey = key;
    fill(listState, problem ? errorPanel(problem) : sessionsState.loaded && !list.length ? emptyPanel("No sessions yet") : null);
    sidebar.replaceChildren(...list.map((e) => {
      const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(e.session === state.open), title: started(e) ? `Started ${fullTime(started(e))}` : "" },
        h("div", { className: "row-title" }, label(e)),
        // Sessions of one workflow step share their name; the run's start time tells them apart.
        started(e) ? h("span", { className: "session-time" }, formatTime(started(e))) : null);
      const pick = () => { location.hash = ChatLogic.sessionHash(e.session); };
      li.addEventListener("click", pick);
      li.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); }
        if (ev.key === "ArrowDown") li.nextElementSibling?.focus();
        if (ev.key === "ArrowUp") li.previousElementSibling?.focus();
      });
      li.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["View run", () => { location.hash = `#runs/${e.trace_id}`; }, "", "Opens the run on Runs"],
        ["View trace", () => { location.hash = `#artifacts?trace=${encodeURIComponent(e.trace_id)}`; }, "", "Opens the run's objects in Blobstore"],
      ]));
      return li;
    }));
  }

  // blank leaves the two session columns with their empty headers, like a page without a selection.
  function blank() {
    state.controller?.destroy();
    state.controller = null;
    state.open = "";
    main.replaceChildren(h("div", { className: "panel-head" }));
    files.replaceChildren(h("div", { className: "panel-head" }), h("div", { className: "panel-body" }));
  }

  // exists asks the blobstore once whether a linked session that does not wait is there at all.
  async function exists(session) {
    if (!probed.has(session)) {
      try {
        const l = await api("GET", `/api/blobs/list/${session}`);
        probed.set(session, (l.directories || []).length + (l.objects || []).length > 0);
      } catch (e) {
        if (e.status === 404 || /not found/i.test(e.message)) probed.set(session, false);
        else return false; // asked again at the next poll
      }
    }
    return probed.get(session);
  }

  // resolve opens the session the hash asks for (or the one last shown). One that does not
  // exist gives way to the newest waiting session; a truly empty list shows the empty state.
  async function resolve() {
    if (state.opening || !sessionsState.loaded) return;
    state.opening = true;
    const asked = state.wanted;
    try {
      const list = entries();
      let entry = list.find((e) => e.session === state.wanted);
      if (!entry && state.wanted && await exists(state.wanted)) {
        // A session that no longer waits: its place is known from its path and its run record.
        try { await runIndex.refresh(); } catch { /* it is named by its trace ID then */ }
        const parts = state.wanted.split("/");
        entry = { session: state.wanted, trace_id: parts[1], step: parts[parts.length - 1], workflow: runIndex.records.get(parts[1])?.workflow || "", waiting: false };
      }
      entry ||= list.find((e) => e.session === state.open) || list[0];
      if (!entry) { if (state.open || !main.childNodes.length) blank(); return; }
      await show(entry);
    } finally {
      state.opening = false;
      renderList();
    }
    if (state.wanted !== asked && state.wanted !== state.open) resolve(); // another session was asked for meanwhile
  }

  async function show(entry) {
    state.wanted = entry.session;
    if (state.open === entry.session) { state.controller?.setClosed(!entry.waiting); syncHash(); return; }
    // Unsaved file edits are asked about before another session replaces this one.
    if (state.controller?.dirty() && !(await confirmAction("Discard unsaved edits?", "Discard"))) {
      state.wanted = state.open;
      syncHash();
      return;
    }
    state.controller?.destroy();
    kept.set(entry.session, entry);
    state.open = entry.session;
    try { localStorage.setItem(LAST_KEY, entry.session); } catch { /* storage unavailable */ }
    state.controller = InteractiveSession.mount(main, {
      session: entry.session, traceId: entry.trace_id, workflowId: entry.workflow_id, workflow: entry.workflow, step: entry.step,
      agent: entry.agent, agents: entry.agents, artifact: entry.artifact, inputs: entry.inputs,
      devPod: ChatLogic.devPodOf(entry), // the run's dev pod goes with every chat message
      filesContainer: files, closed: !entry.waiting,
      // The menu badge and the run's waiting state follow at once, not at the next poll.
      onComplete: () => {
        sessionsState.pending = sessionsState.pending.filter((p) => p.session !== entry.session);
        showSessionsBadge();
        setTimeout(refreshSessions, 2000);
      },
    });
    syncHash();
    reserveToggles();
  }

  function syncHash() {
    if (activeTab !== "sessions" || !state.open) return;
    const want = ChatLogic.sessionHash(state.open);
    if (location.hash !== want) history.replaceState(null, "", want);
  }

  async function refresh() {
    if (document.hidden) return;
    await refreshSessions();
    renderList();
    resolve();
  }

  document.addEventListener("sessionschange", () => { if (activeTab === "sessions") { renderList(); resolve(); } });
  window.addEventListener("beforeunload", (ev) => { if (state.controller?.dirty()) ev.preventDefault(); });

  tabs.sessions = {
    show(rest) {
      let last = "";
      try { last = localStorage.getItem(LAST_KEY) || ""; } catch { /* storage unavailable */ }
      state.wanted = ChatLogic.sessionFromHash(rest) || state.open || (ChatLogic.validSession(last) ? last : "");
      if (!main.childNodes.length) blank();
      renderList();
      resolve();
      if (state.timer) return;
      refresh();
      // No redraw while a right-click menu is open, so its outlined row stays.
      state.timer = setInterval(() => { if (menu.hidden) refresh(); }, POLL_MS);
    },
    hide() {
      clearInterval(state.timer);
      state.timer = null;
    },
  };
})();
