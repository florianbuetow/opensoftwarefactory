"use strict";
// Connectors page: what the factory connects to outside itself. One kind for now, Git: the
// repositories agents may work on, and the live dev pods (a sandbox with a clone of one
// repository on a work branch). Hash: #connectors. The read-only view of one dev pod is the
// sub-page #devpods/<id>?path=<file> (or ?view=log, ?view=diff), opened from here and from Runs.
// Everything goes through /api/connectors/..., the factory UI's proxy to the connector service.

// ---- Logic without the DOM (covered by connectors_ui_test.mjs) ----

const ConnectorsLogic = (() => {
  const KINDS = [["plain", "Plain"], ["github", "GitHub"]];
  const CREDENTIALS = [["none", "No credential"], ["read", "Read"], ["read-write", "Read and write"]];
  const REPOSITORY_NAME = /^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$/;
  const word = (pairs, value, fallback) => (pairs.find(([v]) => v === value) || [])[1] || fallback;
  const kindWord = (kind) => word(KINDS, kind, String(kind || ""));
  // accessWord says what the stored credential allows: "Read", "Read and write" or "No credential".
  const accessWord = (credential) => word(CREDENTIALS, credential, "No credential");

  // items takes a list answer: a bare array, or an object that holds the array under key.
  const items = (answer, key) => (Array.isArray(answer) ? answer : Array.isArray(answer?.[key]) ? answer[key] : []).filter((x) => x && typeof x === "object");
  const text = (value) => (typeof value === "string" ? value : value === undefined || value === null ? "" : String(value));
  const time = (value) => (typeof value === "string" && !value.startsWith("0001") && !Number.isNaN(Date.parse(value)) ? value : "");

  // repositories reads GET /api/connectors/git/repositories: an array (or { repositories: [...] }) of
  //   { name, kind: "github" | "plain", url, default_branch, setup_command, idle_minutes,
  //     approve_publish, credential: "none" | "read" | "read-write" }
  // The token is never part of an answer. Sorted by name.
  function repositories(answer) {
    return items(answer, "repositories").filter((r) => r.name).map((r) => ({
      name: text(r.name), kind: r.kind === "github" ? "github" : "plain", url: text(r.url),
      defaultBranch: text(r.default_branch) || "main", setupCommand: text(r.setup_command),
      idleMinutes: Number(r.idle_minutes) > 0 ? Math.floor(Number(r.idle_minutes)) : 30,
      approvePublish: typeof r.approve_publish === "boolean" ? r.approve_publish : r.kind === "github",
      credential: ["read", "read-write"].includes(r.credential) ? r.credential : "none",
    })).sort((a, b) => a.name.localeCompare(b.name));
  }

  // Browser links leave the stored clone URL intact for the connector and its dev pods.
  function repositoryLink(raw) {
    const githubSSH = /^git@github\.com:([\w.-]+\/[\w.-]+)$/i.exec(raw);
    let url;
    try { url = new URL(githubSSH ? `https://github.com/${githubSSH[1]}` : raw); }
    catch { return ""; }
    if (url.protocol === "ssh:" && url.hostname === "github.com" && /^\/[\w.-]+\/[\w.-]+$/.test(url.pathname)) {
      return `https://github.com${url.pathname.replace(/\.git$/, "")}`;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") return "";
    if (url.host === "git-host:8080") {
      return /^\/[A-Za-z0-9][A-Za-z0-9._-]*\.git$/.test(url.pathname) ? `/git/browse${url.pathname}` : "/git/";
    }
    if (url.hostname === "github.com") url.pathname = url.pathname.replace(/\.git$/, "");
    url.username = "";
    url.password = "";
    return url.href;
  }

  // devPods reads GET /api/connectors/devpods: an array (or { devpods: [...] }) of
  //   { id, repository, name, source_branch, work_branch, runs: [<run ID>], last_activity, expires, phase }
  // runs are the open runs that hold the pod; expires is when it is deleted without one.
  function devPods(answer) {
    return items(answer, "devpods").filter((p) => p.id).map((p) => ({
      id: text(p.id), repository: text(p.repository), name: text(p.name),
      sourceBranch: text(p.source_branch), workBranch: text(p.work_branch),
      runs: (Array.isArray(p.runs) ? p.runs : []).map(text).filter(Boolean),
      lastActivity: time(p.last_activity), expires: time(p.expires), phase: text(p.phase) || "Unknown",
    }));
  }

  // fileList reads GET /api/connectors/devpods/<id>/files?path=:
  //   { path, entries: [{ name, type: "dir" | "file", size }], truncated }
  // Folders first, then files, each by name.
  function fileList(answer) {
    const entries = items(answer, "entries").filter((e) => e.name).map((e) => ({ name: text(e.name), dir: e.type === "dir", size: Number(e.size) || 0 }));
    entries.sort((a, b) => Number(b.dir) - Number(a.dir) || a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }));
    return { path: text(answer?.path), entries, truncated: answer?.truncated === true };
  }

  // fileContent reads GET /api/connectors/devpods/<id>/file?path=: { path, content, note }; note
  // is there when the content was cut.
  const fileContent = (answer) => ({ path: text(answer?.path), content: text(answer?.content), note: text(answer?.note) });

  // gitOutput reads GET /api/connectors/devpods/<id>/log and /diff: { output: "<git's text>" }.
  const gitOutput = (answer) => text(typeof answer === "string" ? answer : answer?.output);

  // repositoryBody is what PUT /api/connectors/git/repositories/<name> takes: the fields of a
  // repository and, only when one was typed, "token". An empty token field leaves the stored
  // token as it is; a repository without a credential keeps none.
  function repositoryBody(form) {
    const body = {
      kind: form.kind === "github" ? "github" : "plain", url: text(form.url).trim(),
      default_branch: text(form.defaultBranch).trim() || "main", setup_command: text(form.setupCommand).trim(),
      idle_minutes: Number(text(form.idleMinutes).trim() || 30),
      approve_publish: !!form.approvePublish,
      credential: ["read", "read-write"].includes(form.credential) ? form.credential : "none",
    };
    const token = text(form.token).trim();
    if (token && body.credential !== "none") body.token = token;
    return body;
  }

  // formProblems says what is wrong with the repository form, by field; empty when fine.
  // existing is the repository being edited, or null for a new one; names are those in use.
  function formProblems(form, existing, names = []) {
    const problems = {};
    const name = text(form.name).trim();
    if (!existing) {
      if (!name) problems.name = "Enter a name.";
      else if (!REPOSITORY_NAME.test(name)) problems.name = "Use lower-case letters, digits, dashes and dots.";
      else if (names.includes(name)) problems.name = `${name} already exists.`;
    }
    if (!text(form.url).trim()) problems.url = "Enter a URL.";
    const idle = text(form.idleMinutes).trim();
    if (idle && !(Number.isInteger(Number(idle)) && Number(idle) >= 1)) problems.idleMinutes = "Enter whole minutes, 1 or more.";
    const stored = existing && existing.credential !== "none";
    if (form.credential !== "none" && !stored && !text(form.token).trim()) problems.token = "Enter a token.";
    return problems;
  }

  // timeLeft says how long a dev pod stays: held while a run that uses it is open, else the
  // time until it expires, in whole minutes.
  function timeLeft(pod, now = Date.now()) {
    if (pod.runs.length) return pod.runs.length === 1 ? "Held by a run" : `Held by ${pod.runs.length} runs`;
    if (!pod.expires) return "";
    const seconds = Math.round((Date.parse(pod.expires) - now) / 1000);
    if (seconds <= 0) return "Expires now";
    return seconds < 60 ? "Under 1 min" : `${Math.floor(seconds / 60)} min`;
  }

  // phaseState maps a pod phase to the state vocabulary, as Cluster does.
  function phaseState(phase) {
    if (phase === "Running") return "done";
    if (phase === "Pending" || phase === "Creating") return "running";
    return phase === "Terminating" ? "idle" : "failed";
  }

  // The view of one dev pod: #devpods/<id>?path=<file>, ?view=log or ?view=diff (the default).
  function podHash(id, { path = "", view = "" } = {}) {
    const query = path ? `path=${encodeURIComponent(path)}` : view && view !== "diff" ? `view=${encodeURIComponent(view)}` : "";
    return `#devpods/${encodeURIComponent(id)}${query ? `?${query}` : ""}`;
  }
  function parsePodHash(rest) {
    const [main, query = ""] = String(rest || "").split("?");
    const params = new URLSearchParams(query);
    let id = main.replace(/^\/+/, "");
    try { id = decodeURIComponent(id); } catch { /* as typed */ }
    const path = params.get("path") || "";
    return { id, path, view: path ? "file" : params.get("view") === "log" ? "log" : "diff" };
  }

  // childPath joins a folder of the pod and a name in it; the root is "" or ".".
  const childPath = (dir, name) => (!dir || dir === "." ? name : `${String(dir).replace(/\/+$/, "")}/${name}`);

  // diffLines splits git's diff into lines with their kind, so additions and removals can be
  // tinted; the + and - at the start of the line still say which is which.
  function diffLines(output) {
    return String(output || "").replace(/\n$/, "").split("\n").map((line) => ({
      text: line,
      kind: /^(diff --git|index |--- |\+\+\+ |new file|deleted file|similarity|rename )/.test(line) ? "head"
        : line.startsWith("@@") ? "hunk" : line.startsWith("+") ? "add" : line.startsWith("-") ? "del" : "",
    }));
  }

  return {
    KINDS, CREDENTIALS, kindWord, accessWord, repositories, repositoryLink, devPods, fileList, fileContent, gitOutput,
    repositoryBody, formProblems, timeLeft, phaseState, podHash, parsePodHash, childPath, diffLines,
  };
})();

// ---- Connectors page ----

(() => {
  const API = "/api/connectors";
  const L = ConnectorsLogic;
  const enc = encodeURIComponent;
  const state = { repos: [], pods: [], loaded: false, error: "", podsLoaded: false, podsError: "", timer: null, key: "" };
  const kinds = $("connector-kinds"), main = $("connector-main");

  const runLabel = (trace) => (runIndex.records.has(trace) ? runIndex.label(trace) : midTruncate(trace, 13));

  async function loadRepositories() {
    try { state.repos = L.repositories(await api("GET", `${API}/git/repositories`)); state.error = ""; }
    catch (e) { state.error = `Cannot list the repositories: ${e.message}`; }
    state.loaded = true;
  }

  async function loadPods() {
    try { state.pods = L.devPods(await api("GET", `${API}/devpods`)); state.podsError = ""; }
    catch (e) { state.podsError = `Cannot list the dev pods: ${e.message}`; }
    state.podsLoaded = true;
  }

  // ---- Repositories ----

  function repositoryTable() {
    if (state.error) return errorPanel(state.error);
    if (!state.loaded) return null;
    if (!state.repos.length) return emptyPanel("No repositories yet");
    return dataTable(["Name", "Kind", "URL", "Default branch", "Access"], state.repos.map((r) => {
      const href = L.repositoryLink(r.url);
      const stop = (event) => event.stopPropagation();
      const link = href ? h("a", { href, target: "_blank", rel: "noopener", onclick: stop, onkeydown: stop, oncontextmenu: stop }, href) : r.url;
      const tr = h("tr", {},
        h("td", {}, r.name),
        h("td", {}, L.kindWord(r.kind)),
        h("td", { className: "name-cell", dataset: { full: href || r.url } }, link),
        h("td", { className: "opt" }, r.defaultBranch),
        h("td", {}, L.accessWord(r.credential)));
      tr.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["Edit…", () => editRepository(r), "", `Changes the settings of ${r.name}`],
        "-",
        ["Delete", () => deleteRepository(r), "danger", `Removes ${r.name} and its stored token`],
        "-",
        ["New repository", () => editRepository(null), "", "Adds a Git repository"],
      ]));
      return clickableRow(tr, () => editRepository(r));
    }), { name: 2, optional: [3] });
  }

  // editRepository opens the form of a repository; null adds a new one. The token is
  // write-only: the field starts empty, and an empty field leaves the stored token as it is.
  function editRepository(repo) {
    const id = (name) => `connector-repo-${name}`;
    const problem = () => h("p", { className: "error-text field-problem", role: "alert", hidden: true });
    const field = (label, control, name, tip = "") => {
      control.id = id(name);
      const says = problem();
      return { box: h("div", { className: "field" }, h("label", { htmlFor: control.id, title: tip || undefined }, label), control, says), says };
    };
    const name = h("input", { value: repo?.name || "", autocomplete: "off", spellcheck: false, disabled: !!repo, title: repo ? "A repository keeps its name; workflows refer to it" : undefined });
    const kind = h("select", {}, ...L.KINDS.map(([value, label]) => h("option", { value, selected: value === (repo?.kind || "plain") }, label)));
    const url = h("input", { value: repo?.url || "", autocomplete: "off", spellcheck: false, placeholder: "https://github.com/owner/name.git" });
    const branch = h("input", { value: repo?.defaultBranch || "", autocomplete: "off", spellcheck: false, placeholder: "main" });
    const setup = h("input", { value: repo?.setupCommand || "", autocomplete: "off", spellcheck: false, placeholder: "No setup command" });
    const idle = h("input", { value: repo ? String(repo.idleMinutes) : "", autocomplete: "off", inputMode: "numeric", placeholder: "30", size: 4, maxLength: 5, className: "connector-idle" });
    const approve = h("input", { type: "checkbox", checked: repo ? repo.approvePublish : false });
    const credential = h("select", {}, ...L.CREDENTIALS.map(([value, label]) => h("option", { value, selected: value === (repo?.credential || "none") }, label)));
    const token = h("input", { type: "password", autocomplete: "new-password", spellcheck: false });
    // A new repository asks before publishing when it is on GitHub, until the box is changed by hand.
    let approveTouched = !!repo;
    approve.addEventListener("change", () => { approveTouched = true; });
    kind.addEventListener("change", () => { if (!approveTouched) approve.checked = kind.value === "github"; });
    const stored = !!repo && repo.credential !== "none";
    const showToken = () => {
      token.disabled = credential.value === "none";
      token.placeholder = credential.value === "none" ? "No token" : stored ? "Unchanged" : "";
      token.title = credential.value === "none" ? "Needs an access other than No credential" : "";
      if (token.disabled) token.value = "";
    };
    credential.addEventListener("change", showToken);
    showToken();
    const fields = {
      name: field("Name", name, "name", "What workflows call the repository: lower-case letters, digits, dashes and dots"),
      kind: field("Kind", kind, "kind", "GitHub opens pull requests through GitHub's API; Plain stores a pull request record with a patch"),
      url: field("URL", url, "url", "The address the dev pod clones from, over HTTP or HTTPS"),
      defaultBranch: field("Default branch", branch, "branch", "The branch a work branch may never be"),
      setupCommand: field("Setup command", setup, "setup", "Runs once in a new dev pod after the clone, such as an install of dependencies"),
      idleMinutes: field("Idle minutes", idle, "idle", "Minutes after the last agent tool call until a dev pod of this repository is deleted"),
      credential: field("Access", credential, "credential", "What the token allows: Read clones a private repository, Read and write also pushes the work branch"),
      token: field("Token", token, "token", "Stored as a secret and never shown again; an empty field keeps the stored token"),
    };
    const approveBox = h("div", { className: "field check-field" },
      h("label", { className: "check", htmlFor: id("approve"), title: "Ticked, a push and a pull request wait on Decisions until a person approves them" }, approve, "Ask a person before publishing"));
    approve.id = id("approve");
    const read = () => ({
      name: name.value, kind: kind.value, url: url.value, defaultBranch: branch.value, setupCommand: setup.value,
      idleMinutes: idle.value, approvePublish: approve.checked, credential: credential.value, token: token.value,
    });
    const save = h("button", { type: "button", className: "primary" }, "Save");
    const cancel = h("button", { type: "button", onclick: () => dlg.close() }, "Cancel");
    const submit = async () => {
      const form = read();
      const problems = L.formProblems(form, repo, state.repos.map((r) => r.name));
      for (const [key, f] of Object.entries(fields)) { f.says.textContent = problems[key] || ""; f.says.hidden = !problems[key]; }
      const first = Object.keys(fields).find((key) => problems[key]);
      if (first) { fields[first].box.querySelector("input, select")?.focus(); return; }
      if (save.disabled) return;
      save.disabled = true;
      try {
        await api("PUT", `${API}/git/repositories/${enc(repo?.name || form.name.trim())}`, L.repositoryBody(form));
        dlg.close();
        setSaveState("Saved");
        await loadRepositories();
        render(true);
      } catch (e) { setStatus(`Not saved: ${e.message}`, "error"); }
      finally { save.disabled = false; }
    };
    save.onclick = submit;
    const body = h("form", { className: "connector-form", onsubmit: (ev) => { ev.preventDefault(); submit(); } },
      fields.name.box, fields.kind.box, fields.url.box, fields.defaultBranch.box, fields.setupCommand.box, fields.idleMinutes.box,
      approveBox, fields.credential.box, fields.token.box,
      h("button", { type: "submit", hidden: true, tabIndex: -1, "aria-hidden": "true" })); // Enter in a field saves
    const dlg = openDialog({ title: repo ? "Edit repository" : "New repository", body, footer: [cancel, save] });
  }

  async function deleteRepository(repo) {
    if (!(await confirmAction(`Delete ${repo.name}?`, "Delete"))) return;
    try {
      await api("DELETE", `${API}/git/repositories/${enc(repo.name)}`);
      setSaveState("Deleted");
    } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
    await loadRepositories();
    render(true);
  }

  // ---- Dev pods ----

  async function destroyPod(pod) {
    if (!(await confirmAction(`Destroy the dev pod on ${pod.workBranch || pod.repository}? Work that was not pushed is lost.`, "Destroy"))) return;
    try {
      await api("DELETE", `${API}/devpods/${enc(pod.id)}`);
      setSaveState("Destroyed");
    } catch (e) { setStatus(`Not destroyed: ${e.message}`, "error"); }
    await loadPods();
    render(true);
  }

  function podTable() {
    if (state.podsError) return errorPanel(state.podsError);
    if (!state.podsLoaded) return null;
    if (!state.pods.length) return emptyPanel("No dev pods");
    const now = Date.now();
    return dataTable(["State", "Repository", "Work branch", "Runs", "Expires", ""], state.pods.map((pod) => {
      const left = L.timeLeft(pod, now);
      const runs = pod.runs.flatMap((trace, i) => [i ? ", " : null, h("a", { href: `#runs/${enc(trace)}`, title: "Opens the run in Runs" }, runLabel(trace))]);
      const destroy = () => destroyPod(pod);
      const tr = h("tr", {},
        h("td", {}, statusChip(L.phaseState(pod.phase), pod.phase)),
        h("td", {}, pod.name ? `${pod.repository} (${pod.name})` : pod.repository),
        h("td", { className: "name-cell", dataset: { full: pod.workBranch } }, pod.workBranch),
        h("td", { className: "opt" }, runs.length ? runs : h("span", { className: "muted" }, "No runs")),
        h("td", { title: pod.expires && !pod.runs.length ? fullTime(pod.expires) : pod.runs.length ? "Kept while a run that uses it is open" : undefined }, left),
        h("td", { className: "fit connector-actions" },
          h("a", { className: "step-action", href: L.podHash(pod.id), title: "Opens the pod's files, its commit log and its diff against the source branch" }, "View files"),
          h("button", { type: "button", className: "danger", title: "Deletes the dev pod now; work that was not pushed is lost", onclick: destroy }, "Destroy")));
      tr.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["View files", () => { location.hash = L.podHash(pod.id); }, "", "Opens the pod's files, its commit log and its diff against the source branch"],
        "-",
        ["Destroy", destroy, "danger", "Deletes the dev pod now; work that was not pushed is lost"],
      ]));
      return tr;
    }), { name: 2, optional: [3] });
  }

  // render draws the page; without force it redraws only when something changed, and never
  // while a menu is open, so the outlined row stays.
  function render(force = false) {
    const key = JSON.stringify([state.repos, state.pods, state.error, state.podsError, state.loaded, state.podsLoaded, state.pods.some((p) => !p.runs.length) ? Math.floor(Date.now() / 60000) : 0,
      state.pods.flatMap((p) => p.runs).map(runLabel)]);
    if (!force && (key === state.key || !menu.hidden)) return;
    state.key = key;
    kinds.replaceChildren(h("li", { role: "option", tabIndex: 0, "aria-selected": "true" }, h("div", { className: "row-title" }, "Git")));
    fill(main,
      h("div", { className: "connector-block" }, h("h3", { className: "section-title connector-title" }, "Repositories"), repositoryTable()),
      h("div", { className: "connector-block" }, h("h3", { className: "section-title connector-title" }, "Dev pods"), podTable()));
  }

  $("connector-new-slot").append(iconButton("plus", "New repository", () => editRepository(null)));
  main.addEventListener("contextmenu", (ev) => {
    if (ev.target.closest("tr")) return;
    openMenu(ev, [["New repository", () => editRepository(null), "", "Adds a Git repository"]]);
  });

  tabs.connectors = {
    show() {
      render(true);
      Promise.all([loadRepositories(), loadPods()]).then(() => render(true));
      runIndex.refresh().then(() => render(), () => {}); // the runs of a pod are named by their workflow
      clearInterval(state.timer);
      // The dev pods are read again every 10 s while the page is open.
      state.timer = setInterval(async () => { if (document.hidden) return; await loadPods(); render(); }, 10000);
    },
    hide() { clearInterval(state.timer); state.timer = null; },
  };
})();

// ---- Dev pod view: the files, the commit log and the diff of one dev pod, read-only ----

(() => {
  const API = "/api/connectors/devpods";
  const L = ConnectorsLogic;
  const enc = encodeURIComponent;
  const LOG = "@log", DIFF = "@diff";
  const state = { id: "", pod: null, gone: false, want: { view: "diff", path: "" }, shown: "", seq: 0, timer: null, treeFor: "" };
  const detail = $("devpod-detail");
  const tree = createTree($("devpod-tree"), {
    onSelect(n) {
      if (n.kind === "dir") return; // a folder only opens
      location.hash = L.podHash(state.id, n.id === LOG ? { view: "log" } : n.id === DIFF ? { view: "diff" } : { path: n.id });
    },
  });

  const gone = (e) => e?.status === 404;
  const fileNodes = (dir, list) => list.entries.map((e) => {
    const path = L.childPath(dir, e.name);
    return e.dir
      ? { id: path, label: e.name, kind: "dir", full: e.name, load: async () => fileNodes(path, L.fileList(await api("GET", `${API}/${enc(state.id)}/files?path=${enc(path)}`))) }
      : { id: path, label: e.name, kind: "file", full: e.name, title: formatSize(e.size) };
  });

  function metaLine() {
    const p = state.pod;
    const sep = () => h("span", { className: "sep" }, "·");
    return h("div", { className: "run-meta" },
      p ? [statusChip(L.phaseState(p.phase), p.phase), sep(), h("span", {}, p.repository), sep(), h("span", { title: `Work branch, created from ${p.sourceBranch}` }, p.workBranch)] : statusChip("idle", "Unknown"));
  }

  function showGone() {
    state.gone = true;
    tree.setRoots([]);
    fill(detail, emptyPanel("This dev pod no longer exists"));
  }

  // loadTree lists the pod's top folder under the two views of its history.
  async function loadTree(seq) {
    const p = state.pod;
    const roots = [
      { id: DIFF, label: p?.sourceBranch ? `Diff against ${p.sourceBranch}` : "Diff", kind: "view" },
      { id: LOG, label: "Commit log", kind: "view" },
    ];
    let files = [], problem = "";
    try { files = fileNodes("", L.fileList(await api("GET", `${API}/${enc(state.id)}/files?path=.`))); }
    catch (e) { if (seq !== state.seq) return; if (gone(e)) { showGone(); return; } problem = e.message; }
    if (seq !== state.seq) return;
    tree.invalidate();
    tree.setRoots([...roots, ...files]);
    if (problem) setStatus(`Cannot list the files: ${problem}`, "error");
    await revealSelection();
  }

  // revealSelection opens the folders above the shown file and marks its row.
  async function revealSelection() {
    const { view, path } = state.want;
    if (view !== "file") { tree.select(view === "log" ? LOG : DIFF); return; }
    const parts = path.split("/");
    await tree.reveal(parts.slice(0, -1).map((_, i) => parts.slice(0, i + 1).join("/")));
    tree.select(path);
  }

  function content(view, path, answer) {
    if (view === "file") {
      const file = L.fileContent(answer);
      const note = file.note ? h("p", { className: "evidence-note" }, file.note) : null;
      if (/\.md$/i.test(path)) {
        const shown = textView(file.content, { detectJSON: false });
        return [h("div", { className: "file-head" }, h("h3", { className: "section-title mono" }, path), shown.toggle), note, shown.body];
      }
      const body = /\.json$/i.test(path) ? pretty(file.content) : file.content;
      return [h("div", { className: "file-head" }, h("h3", { className: "section-title mono" }, path)), note, h("div", { className: "file-block code source" }, body)];
    }
    const output = L.gitOutput(answer);
    if (view === "log") {
      return [h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Commit log")),
        output.trim() ? h("div", { className: "file-block code source" }, output) : h("p", { className: "muted" }, "No commits")];
    }
    const source = state.pod?.sourceBranch;
    return [h("div", { className: "file-head" }, h("h3", { className: "section-title" }, source ? `Diff against ${source}` : "Diff")),
      output.trim()
        ? h("div", { className: "file-block code source diff" }, ...L.diffLines(output).map((line) => h("span", { className: `diff-line${line.kind ? ` diff-${line.kind}` : ""}` }, `${line.text}\n`)))
        : h("p", { className: "muted" }, "No changes")];
  }

  // loadView reads what the hash names and shows it; a quiet reload redraws only what changed.
  async function loadView(seq, quiet = false) {
    const { view, path } = state.want;
    const url = view === "file" ? `${API}/${enc(state.id)}/file?path=${enc(path)}` : `${API}/${enc(state.id)}/${view}`;
    let answer;
    try { answer = await api("GET", url); }
    catch (e) {
      if (seq !== state.seq) return;
      if (gone(e)) { showGone(); return; }
      if (!quiet) fill(detail, h("div", { className: "section" }, metaLine(), errorState(`Cannot read ${view === "file" ? path : view === "log" ? "the commit log" : "the diff"}: ${e.message}`)));
      return;
    }
    if (seq !== state.seq) return;
    const key = JSON.stringify([view, path, answer, state.pod]);
    if (key === state.shown) return;
    state.shown = key;
    fill(detail, h("div", { className: "section" }, metaLine(), content(view, path, answer)));
  }

  async function open(seq) {
    // The pod's repository and branches come from the list of live pods; a pod that is not
    // listed is gone.
    try {
      const pods = L.devPods(await api("GET", API));
      if (seq !== state.seq) return;
      state.pod = pods.find((p) => p.id === state.id) || null;
      if (!state.pod) { showGone(); return; }
    } catch (e) {
      if (seq !== state.seq) return;
      fill(detail, errorPanel(`Cannot load the dev pod: ${e.message}`));
      return;
    }
    state.gone = false;
    if (state.treeFor !== state.id) { state.treeFor = state.id; await loadTree(seq); } else await revealSelection();
    if (seq !== state.seq || state.gone) return;
    await loadView(seq);
  }

  tabs.devpods = {
    show(rest) {
      const at = L.parsePodHash(rest);
      if (!at.id) { location.replace("#connectors"); return; }
      const seq = ++state.seq;
      if (at.id !== state.id) { state.id = at.id; state.pod = null; state.treeFor = ""; state.shown = ""; tree.setRoots([]); fill(detail); }
      state.want = { view: at.view, path: at.path };
      open(seq);
      clearInterval(state.timer);
      // What is shown is read again every 10 s: an agent may be changing the pod meanwhile.
      state.timer = setInterval(() => { if (!document.hidden && !state.gone && state.pod) loadView(state.seq, true); }, 10000);
    },
    hide() { clearInterval(state.timer); state.timer = null; state.seq++; state.treeFor = ""; },
  };
})();
