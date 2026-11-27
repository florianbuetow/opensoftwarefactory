"use strict";
(() => {
  const state = { session: null, events: [], active: false, following: true, busy: false, generation: 0, timer: null };
  const body = measurementUI.panel("events");
  const query = h("input", { id: "events-query", type: "search" });
  const start = h("input", { id: "events-start", type: "datetime-local" });
  const sequence = h("input", { id: "events-sequence", inputMode: "numeric" });
  const status = h("p", { className: "muted", role: "status" });
  const list = h("div", { className: "event-feed", tabIndex: 0, "aria-label": "Workflow events" });
  const detail = h("pre", { className: "measurement-json", hidden: true });
  const follow = h("button", { type: "button", onclick: () => { state.following = !state.following; follow.textContent = state.following ? "Pause" : "Follow"; if (state.following) poll(); } }, "Pause");
  const form = h("form", { className: "measurement-filters", onsubmit: e => { e.preventDefault(); open(false); } },
    measurementUI.field("Search", query), measurementUI.field("From time", start), measurementUI.field("From sequence", sequence),
    h("button", { type: "submit", className: "primary" }, "Rewind"), follow,
    h("button", { type: "button", onclick: () => { start.value = ""; sequence.value = ""; open(true); } }, "Live"));
  // The feed is a table: a heading row, then one row per event with its time, workflow and outcome.
  const table = h("div", { className: "event-table" },
    h("div", { className: "event-feed-head", "aria-hidden": "true" }, h("span", {}, "Time"), h("span", {}, "Workflow"), h("span", {}, "Outcome")), list);
  body.append(form, status, table, detail);
  const when = (iso) => { const at = new Date(iso); return iso && Number.isFinite(+at) ? at.toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "Unknown time"; };
  const named = (id) => typeof titleCase === "function" ? titleCase(id) : id;
  const OUTCOMES = { completed: "done", succeeded: "done", done: "done", failed: "failed", error: "failed", running: "running", started: "running", waiting: "waiting", canceled: "idle", cancelled: "idle" };
  // outcome shows a known state as the factory's state chip (dot and word), anything else as its own word.
  function outcome(event) {
    const word = event.status || event.type || "Unknown outcome", state = OUTCOMES[String(word).toLowerCase()];
    return state && typeof statusChip === "function" ? statusChip(state, named(word)) : named(word);
  }
  let chosen = "";
  list.onscroll = () => { if (list.scrollHeight - list.scrollTop - list.clientHeight > 60) { state.following = false; follow.textContent = "Follow"; } };
  async function close(id) { if (id) await api("DELETE", `/api/connectors/events/sessions/${encodeURIComponent(id)}`).catch(() => {}); }
  async function open(live) {
    const generation = ++state.generation;
    const previous = state.session; state.session = null; state.events = []; detail.hidden = true;
    await close(previous);
    const payload = { query: query.value.trim() };
    if (!live && sequence.value) {
      if (!/^\d+$/.test(sequence.value) || !Number.isSafeInteger(Number(sequence.value))) { status.textContent = "Enter a valid sequence."; return; }
      payload.start_sequence = Number(sequence.value);
    } else if (!live && start.value) payload.start_time = new Date(start.value).toISOString();
    try {
      const result = await api("POST", "/api/connectors/events/sessions", payload);
      const id = result.id || result.session_id;
      if (!state.active || generation !== state.generation) { await close(id); return; }
      state.session = id; state.following = true; follow.textContent = "Pause"; await poll();
    } catch (error) { if (generation === state.generation) status.textContent = `Cannot open events: ${error.message}`; }
  }
  async function poll() {
    if (!state.active || !state.session || state.busy) return;
    state.busy = true; const generation = state.generation;
    try {
      const data = await api("GET", `/api/connectors/events/sessions/${encodeURIComponent(state.session)}?${new URLSearchParams({ limit: "1000", query: query.value.trim() })}`);
      if (generation !== state.generation || !state.active) return;
      const anchor = !state.following ? [...list.children].find(row => (row.offsetTop ?? 0) >= list.scrollTop) : null;
      const anchorID = anchor?.dataset?.eventId, anchorOffset = anchor ? list.scrollTop - anchor.offsetTop : 0;
      const seen = new Set(state.events.map(e => e.id));
      state.events = [...state.events, ...(data.events || []).filter(e => !seen.has(e.id))].slice(-1000);
      list.replaceChildren(...state.events.map(event => {
        const row = h("button", { type: "button", className: "event-feed-row", "aria-pressed": String(event.id === chosen), dataset: { eventId: event.id }, onclick: () => {
          chosen = event.id;
          for (const other of list.children) other.setAttribute?.("aria-pressed", String(other === row));
          detail.hidden = false; detail.textContent = JSON.stringify(event, null, 2);
        } },
        h("time", { title: event.occurred_at || "" }, when(event.occurred_at)), h("span", { title: event.workflow || "" }, event.workflow ? named(event.workflow) : "Unknown workflow"), h("span", {}, outcome(event)));
        return row;
      }));
      if (!state.events.length) list.replaceChildren(emptyPanel("No events"));
      status.textContent = `${state.events.length} / 1,000 displayed · Stream ${data.stream_first_sequence ?? "unknown"}–${data.stream_last_sequence ?? "unknown"}${data.has_more ? " · More retained events" : ""}${data.next_sequence ? ` · Next ${data.next_sequence}` : ""}`;
      if (state.following) list.scrollTop = list.scrollHeight;
      else if (anchorID) {
        const retained = [...list.children].find(row => row.dataset?.eventId === anchorID);
        if (retained) list.scrollTop = retained.offsetTop + anchorOffset;
        else status.textContent += " · Earlier rows left the display; rewind to read them";
      }
    } catch (error) { if (generation === state.generation) status.textContent = `Cannot read events: ${error.message}`; }
    finally { state.busy = false; }
  }
  tabs.events = { show() { state.active = true; if (!state.session) open(true); clearInterval(state.timer); state.timer = setInterval(poll, 2000); }, hide() { state.active = false; ++state.generation; clearInterval(state.timer); const id = state.session; state.session = null; close(id); } };
})();
