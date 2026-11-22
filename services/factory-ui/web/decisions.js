"use strict";
// Decisions page, laid out like Runs: workflows in the sidebar, a table of decisions in
// the middle (pending above decided, each newest first), and the selected decision on the
// right with the question, the input and step outputs it was based on, and either
// Approve / Reject or the outcome. Pending gates are polled every 3 seconds; decided ones
// are the gate records stored in the runs (runs/<trace>/<step>).
// Hash: #decisions/<id>?workflow=<name>; ids are "p:<workflow id>|<step>" and "d:<record id>".

// ---- Logic without the DOM (covered by teams_ui_test.mjs) ----

const DecisionsLogic = {
  // select keeps the page's selection, { selected, seen, none }, in step with what is listed.
  // A decision named by the hash (opened) is shown or nothing is: while it is not listed (its list
  // still loads, the gate does not wait yet, it does not exist) it stays selected with an empty
  // detail, and no other item ever takes its place. An item that was shown here (seen) and is no
  // longer listed leaves nothing selected (none), until the next link or click. Only a bare
  // #decisions selects the first item.
  opened(id) { return { selected: id || "", seen: "", none: false }; },
  select(s, listed, first) {
    if (s.selected && listed) return { selected: s.selected, seen: s.selected, none: false };
    if (s.none || (s.selected && s.seen === s.selected)) return { selected: "", seen: s.seen, none: true };
    return { selected: s.selected || first || "", seen: s.seen, none: false };
  },

  // recordIds names the objects a decision on a gate can be recorded as, the likeliest first: a
  // rejection that goes back to the team is kept beside the gate's record as <artifact>.<round>
  // (the last rejection the orchestrator allows ends the run and is the final record instead);
  // every other decision is the gate's final record, <artifact>.
  recordIds(artifact, round, decision, sentBack) {
    return decision === "rejected" && sentBack ? [`${artifact}.${Math.max(1, Number(round) || 1)}`, artifact] : [artifact];
  },

  // inputDocs lists the documents a gate shows: one per item of its inputs
  // ([{source, artifact, round}]), in their order. The run input is left out, since the page
  // shows it first. repeated marks an item whose source appears more than once (after a merge
  // or batch), so its title names the round.
  inputDocs(inputs) {
    const items = (Array.isArray(inputs) ? inputs : [])
      .filter((item) => item && typeof item.source === "string" && item.source && item.source !== "input" && typeof item.artifact === "string" && item.artifact);
    const times = new Map();
    for (const item of items) times.set(item.source, (times.get(item.source) || 0) + 1);
    return items.map((item) => ({ source: item.source, artifact: item.artifact, round: Math.max(1, Math.floor(Number(item.round)) || 1), repeated: times.get(item.source) > 1 }));
  },

  // publish is the one place that reads a publish decision: a pending entry of
  // GET /api/orchestrator/decisions (or its stored record) whose step is "<step>.publish-<n>" and
  // which carries "publish": { step, pod, n, kind: "push" | "pull_request", title, body, repository, branch }.
  // An agent asked to push its work branch or to open a pull request, and the repository wants
  // a person to approve that. Returns null for every other decision.
  publish(d) {
    const p = d?.publish;
    if (!p || typeof p !== "object") return null;
    const word = (value) => (typeof value === "string" ? value : "");
    const kind = p.kind === "pull_request" ? "pull_request" : "push";
    return {
      step: word(p.step) || String(d.step || "").replace(/\.publish-[0-9]+$/, ""), kind, word: kind === "push" ? "Push" : "Pull request",
      title: word(p.title), body: word(p.body), repository: word(p.repository), branch: word(p.branch),
    };
  },

  // blocked is the reason Approve and Reject are disabled, "" when they are not.
  blocked(name, documentsLoaded) {
    return !name ? "Needs your name in the top bar" : !documentsLoaded ? "Needs the documents to load" : "";
  },
};

(() => {
  const API = "/api/orchestrator/decisions";
  const POLL_MS = 3000;
  const HIDE_KEY = "factory.decisions.hidden";
  const state = { filter: "", selected: "", shown: "", timer: null, pastTimer: null, banner: "", loaded: { pending: false, decided: false } };
  const pending = new Map(); // id -> { id, kind: "pending", d, workflow, step, question, trace, inputs, since }
  const past = new Map(); // id -> { id, kind: "decided", rec, workflow, step, question, trace, inputs, at }
  const decided = new Set(); // pending gates decided here (ID and round), so a poll in flight cannot bring them back
  const deciding = new Set(); // gates whose decision is on its way (ID and round), so a poll that no longer lists them cannot drop them
  const scanned = new Set(); // finished runs whose gate records are loaded
  const sidebar = $("decision-workflows");
  const tableBox = $("decision-table");
  const detail = $("decision-detail");

  const pendingId = (d) => `p:${d.workflow_id}|${d.step}`;
  // A gate that sends rejections back to a team (on_reject) waits again after each of them, under
  // the same ID; its round tells the reviews apart.
  const roundKey = (d) => `${pendingId(d)}#${d.pass || 0}.${d.round || 1}`; // a gate that runs in passes waits again in every pass
  const returns = (c) => c.kind === "pending" && !!c.d.on_reject;

  // ---- Hidden decided items (removed or cleared in this browser) ----

  function hidden() {
    try { return JSON.parse(localStorage.getItem(HIDE_KEY) || "{}"); } catch { return {}; }
  }
  function saveHidden(value) { try { localStorage.setItem(HIDE_KEY, JSON.stringify(value)); } catch { /* storage unavailable */ } }
  function isHidden(c) {
    if (c.kind !== "decided") return false;
    const value = hidden();
    const cutoff = [value.cleared?.[""], value.cleared?.[c.workflow]].filter(Boolean).sort().pop();
    return (value.ids || []).includes(c.id) || (!!cutoff && String(c.at) <= cutoff);
  }
  // clearItem is the list-wide menu item: on the sidebar, and last in every row menu as on Runs.
  const clearItem = (workflow) => ["Clear decisions", () => clearDecided(workflow), "danger", workflow ? `Removes the decided ${titleCase(workflow)} gates from this list` : "Removes every decided gate from this list"];
  // clearDecided hides the decided gates of a workflow ("" for all), after one confirmation like Clear runs.
  async function clearDecided(workflow) {
    const cutoff = new Date().toISOString(); // a gate decided while the dialog is open stays
    const gone = new Set([...past.values()].filter((c) => (!workflow || c.workflow === workflow) && !isHidden(c)).map((c) => c.id));
    if (!gone.size) return;
    if (!(await confirmAction(gone.size === 1 ? "Remove this decision from the list?" : `Remove ${gone.size} decisions from the list?`, "Remove"))) return;
    const value = hidden();
    value.cleared = { ...(value.cleared || {}), [workflow]: cutoff };
    hide(value, gone);
    setSaveState(gone.size === 1 ? "Removed" : `Removed ${gone.size} decisions`);
  }
  async function removeDecided(c) {
    if (!(await confirmAction("Remove this decision from the list?", "Remove"))) return;
    const value = hidden();
    value.ids = [...new Set([...(value.ids || []), c.id])];
    hide(value, new Set([c.id]));
    setSaveState("Removed");
  }
  // hide stores the hidden gates; a hidden selected row hands the selection to its neighbour: the next, else the previous.
  function hide(value, gone) {
    const list = items();
    const at = list.findIndex((c) => c.id === state.selected);
    if (at >= 0 && gone.has(state.selected)) {
      const neighbour = list.slice(at + 1).find((c) => !gone.has(c.id)) || list.slice(0, at).reverse().find((c) => !gone.has(c.id));
      state.selected = neighbour?.id || "";
      history.replaceState(null, "", hashFor());
    }
    saveHidden(value);
    renderAll();
  }

  // ---- Items ----

  // A pending gate waits since its run started (or its last input arrived).
  const time = (c) => c.since || c.at || runIndex.records.get(c.trace)?.started || "";
  const newest = (a, b) => String(time(b)).localeCompare(String(time(a)));

  // items lists what the table shows: pending first, then decided; each newest first.
  function items() {
    const visible = (c) => (!state.filter || c.workflow === state.filter) && !isHidden(c);
    return [...[...pending.values()].filter(visible).sort(newest), ...[...past.values()].filter(visible).sort(newest)];
  }
  const find = (id) => pending.get(id) || past.get(id);

  function chip(c) {
    if (c.kind === "pending") return statusChip("waiting", "Waiting");
    return c.rec.decision === "approved" ? statusChip("done", "Approved") : statusChip("failed", "Rejected");
  }

  function hashFor(id = state.selected) {
    return `#decisions${id ? "/" + encodeURIComponent(id) : ""}${state.filter ? "?workflow=" + encodeURIComponent(state.filter) : ""}`;
  }

  // ---- Sidebar ----

  function renderSidebar() {
    const names = [...new Set([...pending.values(), ...past.values()].filter((c) => !isHidden(c)).map((c) => c.workflow).filter(Boolean))].sort();
    sidebar.replaceChildren(...[["", "All workflows"], ...names.map((n) => [n, titleCase(n)])].map(([value, label]) => {
      const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(state.filter === value) }, h("div", { className: "row-title" }, label));
      const pick = () => { state.filter = value; state.selected = ""; location.hash = hashFor(""); };
      li.addEventListener("click", pick);
      li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
      li.addEventListener("contextmenu", (ev) => openMenu(ev, [clearItem(value)]));
      return li;
    }));
  }

  // ---- Table ----

  function menuFor(c) {
    const run = ["View run", () => { location.hash = `#runs/${c.trace}`; }, "", "Opens the run on Runs"];
    const trace = ["View trace", () => { location.hash = `#artifacts?trace=${encodeURIComponent(c.trace)}`; }, "", "Opens the run's objects in Blobstore"];
    if (c.kind === "pending") {
      // The gate shown in the detail is not decided from here either while its documents still load.
      const tip = DecisionsLogic.blocked(creator(), state.shown !== c.id || state.ready === c.id);
      // A rejection that goes back to the team needs its reason, which is typed in the detail.
      const reject = returns(c) ? ["Reject", () => {}, "danger", tip || "Needs a reason", true] : ["Reject", () => decide(c, "rejected", ""), "danger", tip, !!tip];
      return [["Approve", () => decide(c, "approved", ""), "", tip, !!tip], reject, "-", run, trace, "-", clearItem(state.filter)];
    }
    return [run, trace, "-", ["Remove decision", () => removeDecided(c), "danger", "Removes this decision from the list"], "-", clearItem(state.filter)];
  }

  function renderTable() {
    const list = items();
    if (state.banner) { tableBox.replaceChildren(errorPanel(state.banner)); return; }
    if (!list.length) { tableBox.replaceChildren(emptyPanel("No decisions yet")); return; }
    const showWorkflow = !state.filter;
    tableBox.replaceChildren(dataTable(["Status", ...(showWorkflow ? ["Workflow"] : []), "Question", "By", "Time"], list.map((c) => {
      const tr = h("tr", { "aria-selected": String(c.id === state.selected) },
        h("td", {}, chip(c)),
        showWorkflow ? h("td", { className: "opt" }, titleCase(c.workflow)) : null,
        h("td", { className: "name-cell", dataset: { full: c.question } }, c.question),
        h("td", { className: "opt" }, c.kind === "decided" ? c.rec.decided_by || "" : ""),
        h("td", {}, time(c) ? timeEl(time(c)) : ""));
      tr.addEventListener("contextmenu", (ev) => openMenu(ev, menuFor(c)));
      return clickableRow(tr, () => { location.hash = hashFor(c.id); });
    }), { optional: showWorkflow ? [1, 3] : [2], name: showWorkflow ? 2 : 1 }));
  }

  // ---- Detail ----

  // inputBlocks shows what the gate presented, in full, like the agent prompt in the node
  // panel: the run input, then one document per input item (pretty-printed when it is JSON). The
  // detail scrolls to read it all. box.loaded settles once every document shows its text (or
  // the reason it cannot load): until then the gate is not decided.
  function inputBlocks(c) {
    const box = h("div");
    const loads = [];
    const block = (title, link) => {
      const view = docView();
      box.append(h("div", { className: "file-head" }, h("h3", { className: "section-title" }, title), link, view.toggle), view.body);
      return view;
    };
    const input = block("Input", null);
    loads.push(loadBlob(`runs/${c.trace}/input`).then((b) => { input.setText(b.text.trim()); }, (e) => { input.setText(`Cannot load the run input: ${e.message}`); }));
    for (const { source, artifact, round, repeated } of DecisionsLogic.inputDocs(c.inputs)) {
      const link = h("a", { href: `#artifacts?trace=${encodeURIComponent(c.trace)}&object=${encodeURIComponent(artifact)}` }, "View output");
      const out = block(repeated ? `${titleCase(source)} · round ${round}` : titleCase(source), link);
      loads.push(loadBlob(artifact).then((b) => {
        const result = TeamsLogic.manifest(b.text);
        if (!result) { out.setText(b.text.trim()); return null; }
        // A team result: its summary, then every result file as a document of its own.
        out.setText(result.summary.trim() || "No summary");
        link.before(h("a", { href: TeamsLogic.teamHash(result.team), title: "Opens the messages between the team's agents and the files they wrote" }, "View messages"));
        const documents = result.files.map((f) => teamDocument(c, f));
        out.body.after(...documents.flatMap((d) => d.nodes));
        return Promise.all(documents.map((d) => d.loaded));
      }, (e) => { out.setText(`Cannot load: ${e.message}`); }));
    }
    box.loaded = Promise.all(loads);
    return box;
  }

  // docView is the shared text view for what a gate shows (inputs, documents, comments), with one
  // addition: an underscore inside a word is part of a name (count_words, test_wordcount.py), never
  // emphasis. They are masked before the text is rendered and put back in what is shown, in the
  // Markdown, plain-text, and JSON views alike, the way chat.js does for messages.
  function docView() {
    const view = textView();
    const unmask = () => {
      const texts = document.createTreeWalker(view.body, NodeFilter.SHOW_TEXT);
      while (texts.nextNode()) if (texts.currentNode.data.includes(ChatLogic.UNDERSCORE_MARK)) texts.currentNode.data = ChatLogic.unmaskUnderscores(texts.currentNode.data);
      for (const link of view.body.querySelectorAll("a[href]")) link.setAttribute("href", ChatLogic.unmaskUnderscores(link.getAttribute("href")));
    };
    view.onMode(unmask); // also after the TXT/MD switch drew it again
    return { body: view.body, toggle: view.toggle, setText(text) { view.setText(ChatLogic.maskWordUnderscores(text)); unmask(); } };
  }

  // teamDocument shows one result file of a team under its name, like the other inputs: Markdown,
  // plain text, and JSON in the shared text view, a code file as code.
  function teamDocument(c, file) {
    const code = TeamsLogic.documentKind(file.name) === "code";
    const view = code ? null : docView();
    const body = code ? h("pre", { className: "file-block code source" }) : view.body;
    const show = (text) => { if (code) body.textContent = text; else view.setText(text); };
    const href = memoryObjectHash(file.artifact, c.trace);
    const loaded = loadBlob(file.artifact).then((b) => show(code ? b.text : b.text.trim()), (e) => show(`Cannot load ${file.name}: ${e.message}`));
    return { loaded, nodes: [h("div", { className: "file-head team-document" }, h("h3", { className: "section-title", dataset: { full: file.name } }, file.name),
      href ? h("a", { href }, "View file") : null, view?.toggle), body] };
  }

  // earlierRounds lists the decisions of the rounds before this one: who decided, and the reason.
  // They are stored beside the gate's record as <artifact>.<n>.
  // place puts the section into the detail once there is something to show.
  function earlierRounds(c, place) {
    const box = h("div", { className: "section decision-rounds" });
    const own = c.kind === "pending" ? { artifact: c.d.artifact || "", round: Infinity } : TeamsLogic.roundOf(c.id.slice(2));
    const before = own.round || Infinity; // a final record is the last round
    if (!own.artifact || (c.kind === "pending" && !(c.d.round > 1))) return;
    (async () => {
      const listing = await api("GET", `/api/blobs/list/${own.artifact.slice(0, own.artifact.lastIndexOf("/"))}`);
      const rounds = TeamsLogic.roundRecords(own.artifact, (listing.objects || []).map((o) => o.id), before);
      const records = (await pool(rounds, 4, async (r) => {
        try { return { ...r, rec: JSON.parse((await loadBlob(r.id)).text) }; } catch { return null; }
      })).filter((r) => r?.rec?.decision);
      if (!records.length) return;
      fill(box, h("h3", { className: "section-title" }, "Earlier rounds"), ...records.flatMap((r) => [
        h("div", { className: "run-meta" },
          r.rec.decision === "approved" ? statusChip("done", "Approved") : statusChip("failed", "Rejected"),
          h("span", { className: "sep" }, "·"), h("span", {}, `Round ${r.round}`),
          r.rec.decided_by ? [h("span", { className: "sep" }, "·"), h("span", { dataset: { full: r.rec.decided_by } }, r.rec.decided_by)] : null,
          r.rec.decided_at ? [h("span", { className: "sep" }, "·"), timeEl(r.rec.decided_at)] : null),
        r.rec.comment ? h("div", { className: "file-block prose" }, r.rec.comment) : null,
      ]));
      place(box);
    })().catch(() => { /* the gate still shows; the earlier rounds stay in the list as decided gates */ });
  }

  function renderDetail() {
    const c = find(state.selected);
    if (!c) { detail.replaceChildren(); state.shown = ""; return; }
    if (state.shown === c.id) { fillTime(c); return; } // keep a comment being typed
    state.shown = c.id;
    state.ready = "";
    const blocks = state.blocks = inputBlocks(c);
    state.timeSlot = h("span", { style: "display: contents" });
    // The gate gets its own line under the question, so the status line stays short, as on Runs.
    // A gate that runs in passes names its pass: a waiting one reports it, a record carries it in its ID (<gate>.pass-<n>).
    const pass = c.kind === "pending" ? c.d.pass || stepPass(c.d.artifact || "").pass : stepPass(TeamsLogic.roundOf(c.id.slice(2)).artifact).pass;
    // A publish decision is named by the step that asked and what it asks for ("Deliver · pull request").
    const publish = DecisionsLogic.publish(c.kind === "pending" ? c.d : c.rec);
    const stepName = publish ? `${stepTitle(publish.step, pass)} · ${publish.word.toLowerCase()}` : stepTitle(c.step, pass);
    const where = state.filter ? stepName : `${titleCase(c.workflow)} › ${stepName}`;
    const gate = h("p", { className: "decision-where", dataset: { full: where } }, where);
    // The round, from the second review on: a waiting gate reports it, a decided record carries it (or its ID does).
    const round = TeamsLogic.roundLabel(c.kind === "pending" ? c.d.round : c.rec.round || TeamsLogic.roundOf(c.id.slice(2)).round);
    const meta = h("div", { className: "run-meta" },
      chip(c),
      round ? [h("span", { className: "sep" }, "·"), h("span", {}, round)] : null,
      c.kind === "decided" && c.rec.decided_by ? [h("span", { className: "sep" }, "·"), h("span", { dataset: { full: c.rec.decided_by } }, c.rec.decided_by)] : null,
      state.timeSlot,
      h("span", { className: "sep" }, "·"), h("span", { className: "meta-pair" }, h("a", { href: `#artifacts?trace=${encodeURIComponent(c.trace)}` }, "View trace"), copyButton(c.trace, "trace ID")));
    let outcome = null;
    if (c.kind === "pending") {
      // A comment is review feedback: it wraps and grows, and Enter starts a new line. Only the
      // buttons decide, so Enter never approves what was meant as a rejection.
      const comment = h("textarea", { id: "decision-comment", rows: 1, autocomplete: "off" });
      comment.addEventListener("input", () => { comment.style.height = "auto"; comment.style.height = `${comment.scrollHeight + 2}px`; });
      const approve = h("button", { type: "button", className: "primary", onclick: () => decide(c, "approved", comment.value.trim()) }, "Approve");
      const reject = h("button", { type: "button", className: "danger", onclick: () => decide(c, "rejected", comment.value.trim()) }, "Reject");
      // A gate that returns rejections to a team asks for the reason: it is the message the team works from.
      const back = returns(c);
      if (back) comment.title = "Sent back to the team as a message to its lead agent when you reject";
      // Both stay disabled, with the reason as their tooltip, until the documents the decision is based on are shown.
      const renderButtons = () => {
        const blocked = DecisionsLogic.blocked(creator(), state.ready === c.id);
        approve.disabled = !!blocked;
        approve.title = blocked || "Records your approval and lets the run continue";
        const reason = blocked || (back && !comment.value.trim() ? "Needs a reason" : "");
        reject.disabled = !!reason;
        reject.title = reason || (back ? "Records your rejection and sends the reason back to the team" : "Records your rejection and ends the run");
      };
      comment.addEventListener("input", renderButtons);
      renderButtons();
      blocks.loaded.then(() => { if (state.blocks === blocks) { state.ready = c.id; renderButtons(); } });
      outcome = h("div", { className: "section" }, h("div", { className: "actions" }, h("div", { className: "field" }, h("label", { htmlFor: comment.id }, back ? "Reason" : "Comment"), comment), reject, approve));
    } else if (c.rec.comment) {
      const comment = docView();
      comment.setText(c.rec.comment);
      outcome = h("div", { className: "section" }, h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Comment"), comment.toggle), comment.body);
    }
    const inputs = h("div", { className: "section" }, blocks);
    // What would be published: the repository and branch, and a pull request's title and text.
    let publishing = null;
    if (publish) {
      const text = publish.body ? docView() : null;
      text?.setText(publish.body);
      publishing = h("div", { className: "section decision-publish" },
        h("div", { className: "file-head" }, h("h3", { className: "section-title" }, publish.word), text?.toggle),
        h("p", { className: "decision-where" }, [publish.repository, publish.branch].filter(Boolean).join(" · ")),
        publish.title ? h("p", { className: "pull-request-title" }, publish.title) : null,
        text?.body);
    }
    detail.replaceChildren(
      h("div", { className: "panel-head" }),
      h("div", { className: "panel-body" },
        h("div", { className: "section" }, h("h3", { className: "question" }, c.question), gate, meta),
        publishing,
        inputs,
        outcome));
    earlierRounds(c, (box) => inputs.after(box));
    fillTime(c);
  }

  // fillTime shows the time once it is known (a new run's record may arrive after the detail).
  function fillTime(c) {
    if (!state.timeSlot || state.timeSlot.childNodes.length || !time(c)) return;
    state.timeSlot.replaceChildren(h("span", { className: "sep" }, "·"), timeEl(time(c)));
  }

  async function decide(c, decision, comment) {
    if (!creator()) { setStatus(`Not ${decision}: no name in the top bar`, "error"); return; }
    if (decision === "rejected" && returns(c) && !comment) { setStatus("Not rejected: no reason", "error"); return; }
    const by = creator(), key = roundKey(c.d);
    if (deciding.has(key)) return;
    // Until the answer arrives the gate stays listed and selected, whatever a poll says in between:
    // the selection then goes from the gate to its record, and is never emptied on the way.
    deciding.add(key);
    try {
      await api("POST", API, { workflow_id: c.d.workflow_id, step: c.d.step, decision, comment, decided_by: by });
      setSaveState(decision === "approved" ? "Approved" : "Rejected");
      decided.add(key);
      pending.delete(c.id);
      // The menu badge and the run's waiting state follow at once, not at the next poll; the
      // server confirms shortly after (the gate leaves its list once the run takes the decision).
      decisionsState.pending = decisionsState.pending.filter((d) => !(d.workflow_id === c.d.workflow_id && d.step === c.d.step));
      showDecisionsBadge();
      setTimeout(refreshDecisions, 2000);
      // The decision is listed at once as its own record, from what was submitted, and the
      // selection goes to it (never to another gate); the stored record replaces it once written.
      const ids = DecisionsLogic.recordIds(c.d.artifact || `runs/${c.trace}/${c.step}`, c.d.round, decision, returns(c));
      const id = `d:${ids[0]}`, at = new Date().toISOString();
      past.set(id, {
        id, kind: "decided", provisional: true, workflow: c.workflow, step: c.step, question: c.question, trace: c.trace, inputs: c.inputs, at,
        rec: { decision, comment, decided_by: by, decided_at: at, step: c.step, round: returns(c) ? c.d.round || 1 : 0 },
      });
      if (state.selected === c.id) {
        state.selected = id;
        state.shown = "";
        if (activeTab === "decisions") history.replaceState(null, "", hashFor());
      }
      scanned.delete(c.trace);
      renderAll();
      followRecord(id, ids);
    } catch (e) {
      setStatus(`Not ${decision}: ${e.message}`, "error");
    } finally {
      deciding.delete(key);
    }
  }

  // followRecord waits for the record of a decision made here and puts it in place of the
  // provisional one; the selection follows if the orchestrator stored it under the other name.
  async function followRecord(id, ids) {
    for (let tries = 0; tries < 30; tries++) {
      await new Promise((done) => { setTimeout(done, 2000); });
      for (const object of ids) {
        if (!(await loadRecord(object))) continue;
        if (`d:${object}` !== id) {
          past.delete(id);
          if (state.selected === id) {
            state.selected = `d:${object}`;
            if (activeTab === "decisions") history.replaceState(null, "", hashFor());
          }
        }
        if (menu.hidden) renderAll();
        return;
      }
    }
  }

  // renderAll draws the page; with nothing selected, the first item is selected.
  function renderAll() {
    const list = items();
    // A linked decision keeps the selection (and the hash) while its list still loads. An item
    // that was shown here and is gone (decided elsewhere, removed) leaves nothing selected: no
    // other gate moves under Approve and Reject on its own.
    const listed = !!find(state.selected) && list.some((c) => c.id === state.selected);
    const next = DecisionsLogic.select(state, listed, list[0]?.id);
    const moved = next.selected !== state.selected;
    Object.assign(state, next);
    // A selection that became empty leaves the hash on the item that was shown: a bare #decisions
    // would select the first item when the page is loaded again or returned to from the menu.
    if (moved && next.selected && activeTab === "decisions") history.replaceState(null, "", hashFor());
    renderSidebar();
    renderTable();
    renderDetail();
  }

  // ---- Loading ----

  async function refresh() {
    if (document.hidden) return;
    try {
      const data = await api("GET", API);
      state.banner = "";
      state.loaded.pending = true;
      const live = new Set(data.decisions.map(pendingId));
      for (const [id, c] of [...pending]) if (!live.has(id) && !deciding.has(roundKey(c.d))) pending.delete(id);
      for (const d of data.decisions) {
        const id = pendingId(d);
        if (decided.has(roundKey(d))) continue;
        // The same gate in a later round replaces the earlier one, and its detail is drawn again.
        if (pending.has(id) && roundKey(pending.get(id).d) === roundKey(d)) continue;
        if (pending.has(id) && state.shown === id) state.shown = "";
        pending.set(id, { id, kind: "pending", d, workflow: d.workflow, step: d.step, question: d.question || titleCase(d.step), trace: d.trace_id, inputs: d.inputs, since: runIndex.records.get(d.trace_id)?.started || "" });
      }
    } catch (e) {
      state.banner = `Cannot load decisions: ${e.message}`;
    }
    renderAll();
  }

  // loadRecord reads one gate record (runs/<trace>/...) into the decided list, in place of a
  // provisional one; false when the object is not a gate record (yet).
  async function loadRecord(object) {
    const id = `d:${object}`;
    if (past.has(id) && !past.get(id).provisional) return true;
    if (!/^runs\/[^/]+\/./.test(object) || object.includes("..")) return false;
    try {
      const rec = JSON.parse((await loadBlob(object)).text);
      if (!rec || !rec.decision || !rec.decided_at || !rec.step) return false;
      const tr = rec.trace_id || object.split("/")[1];
      past.set(id, { id, kind: "decided", rec, workflow: rec.workflow || runIndex.records.get(tr)?.workflow || "", step: rec.step, question: rec.question || titleCase(rec.step), trace: tr, inputs: rec.inputs, at: rec.decided_at });
      return true;
    } catch { return false; /* not a gate record */ }
  }

  // loadPast reads the gate records of the runs: objects under runs/<trace>/ that no agent
  // wrote and that hold a decision. Finished runs are read once. Listing a run's objects is slow,
  // so the listings go through the shared queue (newest run first), one scan at a time, only
  // while the page is shown, and the list grows as they arrive.
  async function loadPast() {
    if (state.scanning) return;
    state.scanning = true;
    try {
      try { await runIndex.refresh(); } catch { /* the list keeps what it has */ }
      const traces = runIndex.list().map((r) => r.trace_id).filter((t) => !scanned.has(t));
      await pool(traces, 4, async (trace) => {
        if (activeTab !== "decisions") return;
        // The status first: a listing asked for once the run has finished is complete, and is kept.
        const st = await runIndex.status(trace, 10000, "");
        const final = st.status ? st.status !== "Running" : !!st.gone;
        let t;
        try { t = await traceListings.ask(trace, { owner: "decisions", final }); } catch { return; }
        const prefix = `runs/${trace}/`;
        const candidates = (t.objects || []).filter((o) => o.id.startsWith(prefix)
          && !/\/(input|run\.json)$/.test(o.id) && !o.id.endsWith(".run.json")
          && !String(o.creator || "").startsWith("agent:"));
        let added = false;
        for (const o of candidates) {
          const known = past.get(`d:${o.id}`);
          if (known && !known.provisional) continue;
          if (await loadRecord(o.id)) added = true;
        }
        if (final) scanned.add(trace);
        if (added && menu.hidden && activeTab === "decisions") renderAll();
      });
      if (activeTab !== "decisions") return;
      state.loaded.decided = true;
      renderAll();
    } finally { state.scanning = false; }
  }

  document.addEventListener("creatorchange", () => { state.shown = ""; renderDetail(); });

  tabs.decisions = {
    show(rest) {
      const [path, query = ""] = rest.split("?");
      state.filter = new URLSearchParams(query).get("workflow") || "";
      // What the hash names is shown, or nothing: never another item in its place.
      Object.assign(state, DecisionsLogic.opened(decodeURIComponent(path.replace(/^\/+/, ""))));
      renderAll();
      // A linked decision is read by its own name, not found by listing every run.
      const linked = state.selected;
      if (linked.startsWith("d:") && !past.has(linked)) loadRecord(linked.slice(2)).then((found) => { if (found && state.selected === linked) renderAll(); });
      if (state.timer) return;
      refresh();
      loadPast();
      // No redraw while a right-click menu is open, so its outlined row stays.
      state.timer = setInterval(() => { if (menu.hidden) refresh(); }, POLL_MS);
      state.pastTimer = setInterval(() => { if (menu.hidden) loadPast(); }, 15000);
    },
    hide() {
      clearInterval(state.timer);
      clearInterval(state.pastTimer);
      state.timer = state.pastTimer = null;
    },
  };
})();
