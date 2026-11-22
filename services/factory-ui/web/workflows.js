"use strict";
// Workflows page: a tree of groups and their workflows, the canvas with the selected workflow's
// graph, and the inspector for the selected step (or the workflow when no step is selected).
// Names are paths, as on Agents and Policies: "ecc/ECC-plan" is the workflow ECC-plan in the
// group ecc. Edits stay on the page until Save stores them as the next version of the workflow;
// stored versions never change. Hash: #workflows/<workflow>/<step>, the workflow encoded as one
// part (ecc%2FECC-plan), followed by ?version=<n> when the version shown is not the newest and by
// path=<root>,<nested> when the workflow was opened through workflow nodes. Workflow references
// pin a version (see workflowref.js).

// ---- Logic without the DOM (covered by teams_editor_test.mjs) ----

const WorkflowLogic = (() => {
  const RESERVED_MEMBER = "workflow"; // the orchestrator's own end of the lead's task channel
  const ONE_WAY = "one-way"; // third entry of a channel: only its first member sends

  // memberName: the last path segment of an agent or workflow name without its version
  // (demo/lead@v1 -> lead, ecc/ECC-plan@v1 -> ECC-plan), as the orchestrator names members.
  const memberName = (ref) => String(ref || "").split("@")[0].split("/").filter(Boolean).pop() || "";

  // teamMembers lists a team step's members: the lead first, then the agents, then the workflows.
  function teamMembers(s) {
    const members = [];
    if (s.agent) members.push({ name: memberName(s.agent), kind: "agent", ref: s.agent, lead: true });
    for (const ref of s.agents || []) members.push({ name: memberName(ref), kind: "agent", ref, lead: false });
    // A workflow member is named by its workflow without its groups and the pinned version.
    for (const ref of s.workflows || []) members.push({ name: memberName(ref), kind: "workflow", ref, lead: false });
    return members;
  }

  // channelId: the two member names in alphabetical order joined by "--".
  const channelId = (a, b) => [String(a), String(b)].sort().join("--");

  // defaultChannels: the lead has a channel to every other member.
  function defaultChannels(s) {
    const [lead, ...others] = teamMembers(s);
    return lead && lead.lead ? others.map((m) => [lead.name, m.name]) : [];
  }

  // teamChannels: the channels a team runs with, each once: the stored ones, or the default.
  function teamChannels(s) {
    const stored = Array.isArray(s.channels) ? s.channels.filter((c) => Array.isArray(c) && (c.length === 2 || c.length === 3)) : [];
    if (!stored.length) return { pairs: defaultChannels(s), isDefault: true };
    const seen = new Set();
    const pairs = [];
    for (const c of stored) {
      const id = channelId(c[0], c[1]);
      if (seen.has(id)) continue;
      seen.add(id);
      pairs.push(c[2] === ONE_WAY ? [c[0], c[1], ONE_WAY] : [c[0], c[1]]);
    }
    return { pairs, isDefault: false };
  }

  // toggleChannel adds the channel between two members, or removes it when it exists. The first
  // edit turns the default into stored channels. Returns the list to store; undefined is the default.
  function channelProblem(s, a, b) {
    const workflows = new Set((s.workflows || []).map(WorkflowRef.nameOf));
    return workflows.has(a) && workflows.has(b) ? "Two workflows cannot share a communication channel; connect each to an agent" : "";
  }

  function toggleChannel(s, a, b) {
    if (!a || !b || a === b) return s.channels;
    const { pairs } = teamChannels(s);
    const id = channelId(a, b);
    if (!pairs.some(([x, y]) => channelId(x, y) === id) && channelProblem(s, a, b)) return s.channels;
    const next = pairs.some(([x, y]) => channelId(x, y) === id) ? pairs.filter(([x, y]) => channelId(x, y) !== id) : [...pairs, [a, b]];
    return next.length ? next : undefined;
  }

  // channelSender: the only member that sends on a one-way channel, or "" when both send.
  const channelSender = (c) => (Array.isArray(c) && c[2] === ONE_WAY ? String(c[0]) : "");

  // setChannelDirection makes the channel between two members one-way from sender, or two-way when
  // sender is "". The first edit turns the default into stored channels. Returns the list to store.
  function setChannelDirection(s, a, b, sender) {
    const id = channelId(a, b);
    return teamChannels(s).pairs.map((c) => (channelId(c[0], c[1]) !== id ? c : sender ? [sender, sender === c[0] ? c[1] : c[0], ONE_WAY] : [c[0], c[1]]));
  }

  // pruneChannels drops stored channels that name someone who is no longer a member.
  function pruneChannels(s) {
    if (!Array.isArray(s.channels)) return undefined;
    const names = new Set(teamMembers(s).map((m) => m.name));
    const next = s.channels.filter((c) => Array.isArray(c) && (c.length === 2 || c.length === 3) && c[0] !== c[1] && names.has(c[0]) && names.has(c[1]));
    return next.length ? next : undefined;
  }

  // teamProblems: why the orchestrator would refuse this team, one sentence each; empty when fine.
  function teamProblems(s) {
    const problems = [];
    const members = teamMembers(s);
    if (!s.agent) problems.push("A team needs a lead agent");
    const byName = new Map();
    for (const m of members) byName.set(m.name, [...(byName.get(m.name) || []), m.ref]);
    for (const [name, refs] of byName) {
      if (!name) problems.push(`${refs[0] || "A member"} has no name`);
      else if (refs.length > 1) problems.push(`Two members would be named ${name}: ${refs.join(" and ")}`);
      if (name === RESERVED_MEMBER) problems.push(`A member cannot be named ${RESERVED_MEMBER}: ${refs[0]}`);
    }
    for (const c of Array.isArray(s.channels) ? s.channels : []) {
      if (!Array.isArray(c) || c.length < 2 || c.length > 3) { problems.push("A channel connects exactly two members"); continue; }
      if (c.length === 3 && c[2] !== ONE_WAY) problems.push(`A channel is two-way or ${ONE_WAY}, not ${c[2]}`);
      if (c[0] === c[1]) problems.push(`A channel connects two different members, not ${c[0]} with itself`);
      for (const name of new Set(c.slice(0, 2))) if (!byName.has(name)) problems.push(`A channel names ${name}, who is not a member`);
      const problem = channelProblem(s, c[0], c[1]);
      if (problem) problems.push(problem);
    }
    return problems;
  }

  // memberTip: what a click on a member tile does, given the member picked before it (if any) and
  // the team's channels. Tooltips carry the guidance; the panel shows no instructions.
  function memberTip(m, picked, pairs) {
    if (!picked) return `Picks ${m.ref} to draw a channel to the member picked next`;
    if (picked.name === m.name) return `Ends the pick of ${m.name}`;
    const id = channelId(picked.name, m.name);
    if (m.kind === "workflow" && picked.kind === "workflow" && !pairs.some(([a, b]) => channelId(a, b) === id)) return "Two workflows cannot share a communication channel; connect each to an agent";
    return `${pairs.some(([a, b]) => channelId(a, b) === id) ? "Removes the" : "Adds a"} channel between ${picked.name} and ${m.name}`;
  }

  // setLead makes an agent the lead. A member that becomes the lead swaps places with the old lead.
  function setLead(s, ref) {
    const agents = [...(s.agents || [])];
    const at = agents.indexOf(ref);
    if (at >= 0) { if (s.agent) agents[at] = s.agent; else agents.splice(at, 1); }
    return { agent: ref, agents };
  }

  // removeMember returns the agents and workflows without the member of that name; the lead stays.
  function removeMember(s, name) {
    return {
      agents: (s.agents || []).filter((ref) => memberName(ref) !== name),
      workflows: (s.workflows || []).filter((ref) => memberName(ref) !== name),
    };
  }

  // nestedRefs: the workflow references of a workflow's workflow steps and team members.
  function nestedRefs(w) {
    const refs = [];
    for (const s of w?.steps || []) {
      if (s.service === "workflow" && s.workflow) refs.push(s.workflow);
      if (s.service === "team") refs.push(...(s.workflows || []));
    }
    return refs;
  }

  // nestedNames: the workflows a workflow starts through workflow steps and team members.
  const nestedNames = (w) => nestedRefs(w).map(WorkflowRef.nameOf);

  // reachesWorkflow reports whether workflow `from` starts workflow `to`, directly or through others.
  function reachesWorkflow(workflows, from, to) {
    const seen = new Set();
    const stack = [from];
    while (stack.length) {
      const name = stack.pop();
      if (name === to) return true;
      if (seen.has(name)) continue;
      seen.add(name);
      stack.push(...nestedNames(workflows.find((w) => w.name === name)));
    }
    return false;
  }

  // nestableWorkflows: the workflows `current` may start without reaching itself.
  const nestableWorkflows = (workflows, current) => workflows.map((w) => w.name).filter((name) => !reachesWorkflow(workflows, name, current));

  // newestRef pins a workflow to its newest version: "name@vN"; the bare name while it has none.
  const newestRef = (workflows, name) => WorkflowRef.format(name, WorkflowRef.newest((workflows || []).find((w) => w.name === name)));

  // versionWord is the pinned version of a reference as text ("v2"), or "" for a bare name.
  const versionWord = (ref) => { const { version } = WorkflowRef.parse(ref); return version ? `v${version}` : ""; };

  // renameWorkflowRefs follows a renamed (or moved) workflow in every workflow step, team, and
  // channel. A reference keeps its pinned version; a channel names its member, the workflow
  // without its groups and the version, so a move into another group leaves channels alone.
  function renameWorkflowRefs(workflows, from, to) {
    const follow = (ref) => { const { name, version } = WorkflowRef.parse(ref); return name === from ? WorkflowRef.format(to, version) : ref; };
    const was = memberName(from), now = memberName(to);
    for (const w of workflows) for (const s of w.steps || []) {
      if (s.service === "workflow" && s.workflow) s.workflow = follow(s.workflow);
      if (s.service !== "team" || !(s.workflows || []).some((ref) => WorkflowRef.nameOf(ref) === from)) continue;
      s.workflows = s.workflows.map(follow);
      if (was === now) continue;
      if (Array.isArray(s.channels)) s.channels = s.channels.map((c) => (Array.isArray(c) ? c.map((name, i) => (i < 2 && name === was ? now : name)) : c));
      // The member's place follows its new name, as the orchestrator stores it.
      if (s.member_positions && was in s.member_positions) {
        s.member_positions = Object.fromEntries(Object.entries(s.member_positions).map(([name, at]) => [name === was ? now : name, at]));
      }
    }
  }

  // versionOptions lists a workflow's versions for a select, newest first: "v3 (newest)", "v2",
  // "v1". A pinned version that is not among them is listed as well, marked missing.
  function versionOptions(versions, pinned = 0) {
    const listed = [...new Set((Array.isArray(versions) ? versions : []).map(Number).filter((n) => n > 0))];
    const newest = listed.length ? Math.max(...listed) : 0;
    const all = pinned > 0 && !listed.includes(pinned) ? [...listed, pinned] : listed;
    return all.sort((a, b) => b - a).map((version) => ({
      version, label: `v${version}${!listed.includes(version) ? " (missing)" : version === newest ? " (newest)" : ""}`,
    }));
  }

  // storable is what Save sends: the definition without the fields the orchestrator sets.
  function storable(w) {
    const { version, versions, created, created_by, ...rest } = w || {};
    // Older definitions may contain line breaks; new writes store a single paragraph.
    if (typeof rest.description === "string") rest.description = rest.description.replace(/\s+/g, " ");
    return rest;
  }

  // Names and descriptions have their own in-place save paths; only graph edits need a new version.
  const fingerprint = (w) => { const { name, description, ...rest } = storable(w); return JSON.stringify(rest); };

  // headState is what the canvas head shows for the workflow on the canvas: the note beside the
  // version select, whether Save is enabled, its tooltip, and the version Save would store.
  // An older version is read-only until it is edited; Save then stores it as the next version.
  function headState({ draft = false, version = 0, newest = 0, dirty = false, steps = 0, saving = false } = {}) {
    const next = draft ? 1 : newest + 1;
    const older = !draft && version > 0 && version < newest;
    const note = draft ? "Not saved yet" : !older ? "" : dirty ? `Edited v${version}: saves as v${next}` : `Viewing v${version}`;
    const blocked = saving ? "Stores the workflow now" : !steps ? "Needs a step" : !dirty ? "No unsaved edits" : "";
    return { next, note, canSave: !blocked, saveTip: blocked || `Stores the workflow as v${next}` };
  }

  // runBlocker says why a run cannot start from the editor, or "": a run uses a stored version,
  // so a workflow with unsaved edits has to be saved first.
  function runBlocker({ draft = false, dirty = false, steps = 0, saving = false } = {}) {
    if (!steps) return "Needs a step";
    if (saving) return "Stores the workflow now";
    return draft || dirty ? "Needs the workflow saved first: a run uses a saved version" : "";
  }

  // ---- Rejections that go back (a gate's on_reject) ----
  // A gate can send a rejection back to an earlier step: an agent, interactive, team or
  // workflow step it depends on, directly or through other steps. That step runs again with
  // the reason, and so do the steps between it and the gate.
  const REJECT_KINDS = ["agent", "interactive", "team", "workflow"];
  const KIND_WORD = { gate: "gate", route: "router", merge: "merge", batch: "batch", devpod: "dev pod", "devpod-stop": "stop dev pod", command: "command" };
  const stepsById = (w) => new Map((w?.steps || []).map((s) => [s.id, s]));

  // ancestors: the steps a step depends on, directly or through others, nearest first.
  function ancestors(w, id) {
    const byId = stepsById(w), seen = new Set([id]), found = [];
    let level = byId.get(id)?.depends_on || [];
    while (level.length) {
      const next = [];
      for (const dep of level) {
        if (seen.has(dep) || !byId.has(dep)) continue;
        seen.add(dep);
        found.push(dep);
        next.push(...(byId.get(dep).depends_on || []));
      }
      level = next;
    }
    return found;
  }

  // dependents: the step and every step that depends on it, directly or through others.
  function dependents(w, id) {
    const set = new Set([id]);
    for (let changed = true; changed;) {
      changed = false;
      for (const s of w?.steps || []) {
        if (!set.has(s.id) && (s.depends_on || []).some((dep) => set.has(dep))) { set.add(s.id); changed = true; }
      }
    }
    return set;
  }

  // rejectRefusal says why a gate cannot send its rejections back to a step, in one sentence,
  // or "" when it can. It follows what the orchestrator refuses on Save. name turns a step ID
  // into what the page shows.
  function rejectRefusal(w, gate, targetId, name = (id) => id) {
    const byId = stepsById(w), target = byId.get(targetId);
    if (!target || targetId === gate.id) return "A rejection goes back to an earlier step";
    if (!REJECT_KINDS.includes(target.service)) return `A rejection cannot go back to ${name(targetId)}, a ${KIND_WORD[target.service] || "service"} step`;
    const before = new Set(ancestors(w, gate.id));
    if (!before.has(targetId)) return `A rejection goes back to a step ${name(gate.id)} depends on, and it does not depend on ${name(targetId)}`;
    const after = dependents(w, targetId);
    // The target and the steps between it and the gate run again.
    const again = (w?.steps || []).filter((s) => s.id !== gate.id && after.has(s.id) && (s.id === targetId || before.has(s.id)));
    const pod = again.find((s) => s.service === "devpod" || s.service === "devpod-stop");
    if (pod) return `The ${KIND_WORD[pod.service]} step ${name(pod.id)} lies between ${name(targetId)} and ${name(gate.id)} and cannot run again`;
    const ids = new Set(again.map((s) => s.id));
    // A merge or batch would receive a result a second time: after a team, any that follows it;
    // otherwise one between the two steps, or one that collects a step that runs again.
    const node = (w?.steps || []).find((s) => isNode(s) && (target.service === "team" ? after.has(s.id) : ids.has(s.id) || (s.depends_on || []).some((dep) => ids.has(dep))));
    if (node) return `A rejection cannot go back across the ${node.service} step ${name(node.id)}`;
    return "";
  }

  // rejectTargets: the steps a gate's rejections can go back to, nearest first.
  function rejectTargets(w, gate) {
    return ancestors(w, gate.id).filter((id) => !rejectRefusal(w, gate, id));
  }

  // backEdgeTip says what goes back along a gate's back edge.
  function backEdgeTip(gate, target, name = (id) => id) {
    const what = target?.service === "team"
      ? `goes to the lead of ${name(gate.on_reject)} as a message with the reason, and the team works on`
      : `goes back to ${name(gate.on_reject)} with the reason, its previous result and the reviewed documents, and it runs again with the steps after it`;
    return `A rejection at ${name(gate.id)} ${what}; a click removes it`;
  }

  // backEdgeRoute decides how a gate's back edge runs to its target (both by their top-left
  // corners; others are the other tiles, size a tile's { w, h }). It leaves the Rejected port
  // on the gate's right edge and enters a side of the target at the same height, along
  // straight lines that keep clear of every tile: beside the tiles in the free channel
  // between two columns and, when a tile stands in the way, through the gap above or below a
  // row. pad is how far the target's outline reaches past its box on the right (a team's
  // stack). Returns the corner points, the path, and whether it is clear of every tile.
  const REJECT_PORT_Y = 20, BACK_LANE = 28, BACK_GAP = 36, BACK_CLEAR = 6, BACK_TIP = 9;
  function backEdgeRoute(gate, target, others, size, pad = 0) {
    const { w, h } = size;
    const tiles = (others || []).filter(Boolean);
    const start = { x: gate.x + w, y: gate.y + REJECT_PORT_Y };
    const endY = target.y + REJECT_PORT_Y;
    const right = target.x + w + pad;
    const near = (a, b, box) => Math.min(a.x, b.x) < box.x + w + BACK_CLEAR && Math.max(a.x, b.x) > box.x - BACK_CLEAR
      && Math.min(a.y, b.y) < box.y + h + BACK_CLEAR && Math.max(a.y, b.y) > box.y - BACK_CLEAR;
    // The first line leaves the gate and the last one enters the target; every other line keeps clear of both.
    const clear = (points) => points.every((p, i) => i === 0 || ![...tiles, ...(i > 1 ? [gate] : []), ...(i < points.length - 1 ? [target] : [])].some((box) => near(points[i - 1], p, box)));
    const length = (points) => points.reduce((sum, p, i) => (i ? sum + Math.abs(p.x - points[i - 1].x) + Math.abs(p.y - points[i - 1].y) : 0), 0);
    // end is where the edge meets the target: its right side from a lane right of it, its left side from one left of it.
    const end = (lane) => (lane > right ? { x: right, y: endY, dir: -1 } : lane < target.x ? { x: target.x, y: endY, dir: 1 } : null);
    const own = gate.x + w + BACK_LANE; // the channel right of the gate
    const candidates = [];
    for (const lane of new Set([own, right + BACK_LANE])) {
      const to = end(lane);
      if (to && lane >= start.x + BACK_LANE / 2) candidates.push([start, { x: lane, y: start.y }, { x: lane, y: endY }, to]);
    }
    for (const lane of [right + BACK_LANE, target.x - BACK_LANE]) {
      const to = end(lane);
      if (!to || lane === own) continue;
      for (const box of [gate, target, ...tiles]) for (const y of [box.y - BACK_GAP, box.y + h + BACK_GAP]) {
        candidates.push([start, { x: own, y: start.y }, { x: own, y }, { x: lane, y }, { x: lane, y: endY }, to]);
      }
    }
    let points = candidates.filter(clear).sort((a, b) => length(a) - length(b))[0];
    const isClear = !!points;
    if (!points) {
      // Nothing is free: around everything on the right, the nearest to "never across a tile".
      const lane = Math.max(right, ...[gate, ...tiles].map((box) => box.x + w)) + BACK_LANE;
      points = [start, { x: lane, y: start.y }, { x: lane, y: endY }, end(lane)];
    }
    const to = points[points.length - 1];
    const corners = [...points.slice(0, -1), { x: to.x - to.dir * BACK_TIP, y: to.y }] // room for the arrowhead
      .filter((p, i, all) => !i || p.x !== all[i - 1].x || p.y !== all[i - 1].y);
    // Straight lines with rounded corners.
    let d = `M${corners[0].x},${corners[0].y}`;
    for (let i = 1; i < corners.length; i++) {
      const p = corners[i], before = corners[i - 1], next = corners[i + 1];
      if (!next) { d += ` L${p.x},${p.y}`; break; }
      const r = Math.min(8, (Math.abs(p.x - before.x) + Math.abs(p.y - before.y)) / 2, (Math.abs(next.x - p.x) + Math.abs(next.y - p.y)) / 2);
      const from = { x: p.x - Math.sign(p.x - before.x) * r, y: p.y - Math.sign(p.y - before.y) * r };
      const on = { x: p.x + Math.sign(next.x - p.x) * r, y: p.y + Math.sign(next.y - p.y) * r };
      d += ` L${from.x},${from.y} Q${p.x},${p.y} ${on.x},${on.y}`;
    }
    return { points: corners, d, clear: isClear };
  }

  // The version and the path of a nested workflow travel in the hash:
  // /<workflow>/<step>?version=<n>&path=<root>,<nested>. version is given only when the version
  // shown is not the newest (0 means the newest); a part of the path is a workflow reference.
  const decode = (text) => { try { return decodeURIComponent(text); } catch { return text; } };
  // The workflow is one part, its slashes encoded (ecc%2FECC-plan); given the names, a grouped
  // name typed with plain slashes (#workflows/ecc/ECC-plan/step) is found among them as well.
  function parseHash(rest, names = []) {
    const [main, query = ""] = String(rest || "").split("?");
    const parts = main.split("/").filter(Boolean).map(decode);
    let take = 1;
    if (parts.length > 1 && !names.includes(parts[0])) for (let n = Math.min(4, parts.length); n > 1; n--) if (names.includes(parts.slice(0, n).join("/"))) { take = n; break; }
    const workflow = parts.slice(0, take).join("/"), step = parts[take] || "";
    const found = /(?:^|&)path=([^&]*)/.exec(query);
    const version = /(?:^|&)version=([1-9][0-9]*)(?:&|$)/.exec(query);
    return { workflow, step, version: version ? Number(version[1]) : 0, path: found ? found[1].split(",").filter(Boolean).map(decode) : [] };
  }
  function formatHash({ workflow, step = "", version = 0, path = [] }) {
    if (!workflow) return "#workflows";
    const query = [version > 0 ? `version=${version}` : "", path.length ? "path=" + path.map(encodeURIComponent).join(",") : ""].filter(Boolean).join("&");
    return `#workflows/${encodeURIComponent(workflow)}${step ? "/" + encodeURIComponent(step) : ""}${query ? "?" + query : ""}`;
  }

  // teamLayout places n member tiles on an ellipse, the first (the lead) at the top: x in percent
  // of the width, y in px. The ellipse grows with the team, so neighbours never overlap.
  const MEMBER_H = 44;
  function teamLayout(n) {
    // A large odd team leaves the place at the bottom free, or the two tiles beside it would touch.
    const slots = n >= 8 && n % 2 ? n + 1 : n;
    const step = slots > 0 ? (2 * Math.PI) / slots : 0;
    const ry = n < 2 ? 0 : n < 8 ? 72 : Math.ceil((MEMBER_H + 8) / (1 - Math.cos(step)));
    const cy = MEMBER_H / 2 + 4 + ry;
    const points = Array.from({ length: n }, (_, i) => {
      const slot = slots !== n && i >= slots / 2 ? i + 1 : i;
      return {
        x: Math.round((50 + (n < 2 ? 0 : 36) * Math.sin(slot * step)) * 100) / 100,
        y: Math.round(cy - ry * Math.cos(slot * step)),
      };
    });
    return { height: Math.round(2 * cy), points };
  }

  // edgeRoute decides how a dependency arrow runs between two tiles (a to b, by their top-left
  // corners; size is a tile's { w, h }). A target below its source keeps the arrow from the
  // bottom port to the top port ("down"). A target beside its source, in the same row, would
  // send that arrow behind both tiles, so it leaves the facing side and enters the facing side
  // ("side", dir 1 to the right, -1 to the left); when other tiles stand in between, it passes
  // under them ("under", y the line it runs along).
  function edgeRoute(a, b, others, size) {
    const { w, h } = size;
    const dir = b.x >= a.x + w ? 1 : b.x + w <= a.x ? -1 : 0;
    if (!dir || b.y - (a.y + h) >= 24) return { kind: "down" };
    const x1 = dir > 0 ? a.x + w : b.x + w, x2 = dir > 0 ? b.x : a.x;
    const y1 = Math.min(a.y, b.y) + h / 2, y2 = Math.max(a.y, b.y) + h / 2;
    const between = (others || []).filter((o) => o && o.x < x2 && o.x + w > x1 && o.y < y2 + 4 && o.y + h > y1 - 4);
    return between.length ? { kind: "under", dir, y: Math.max(a.y, b.y, ...between.map((o) => o.y)) + h + 24 } : { kind: "side", dir };
  }

  // ---- Merge and batch nodes ----
  // A merge waits for a result from every source and forwards them together. A batch collects
  // results from any source and forwards them together once it holds its size. Both only route.
  const NODE_KINDS = ["merge", "batch"];
  const isNode = (s) => NODE_KINDS.includes(s?.service);
  // The parameters of each kind, with the default the orchestrator uses when one is left out.
  const NODE_PARAMS = { merge: { capacity: 16, input_timeout: 300 }, batch: { size: null, input_timeout: 300 } };
  const PARAM_NAME = { capacity: "Capacity", size: "Size", input_timeout: "Input timeout" };
  const KEPT_FIELDS = ["id", "service", "depends_on", "position"]; // what every kind of step has

  // nodeSubtitle is the second line of a merge or batch tile.
  const nodeSubtitle = (s) => (s.service === "merge" ? "Waits for all" : Number(s.size) > 0 ? `Collects ${s.size}` : "Collects");

  // ---- Dev pod steps ----
  // A devpod step starts the run's dev pod (a sandbox with a clone of a repository on a work
  // branch), a command step runs one command in it without a model, and a devpod-stop step
  // deletes it.
  const POD_KINDS = ["devpod", "command", "devpod-stop"];
  const isPodKind = (s) => POD_KINDS.includes(s?.service);
  const DEFAULT_WORK_BRANCH = "agent/{run}"; // {run} stands for the first 8 characters of the run ID
  const RULE_TOOLS = ["write_file", "edit_file", "run_command"];
  // What each kind has beyond the fields of every step, and the fields only these kinds have.
  const POD_FIELDS = {
    devpod: ["repository", "source_branch", "work_branch", "name", "protected", "rules", "timeout", "max_attempts"],
    command: ["command", "timeout", "allow_failure"],
    "devpod-stop": [],
  };
  const POD_ONLY = ["repository", "source_branch", "work_branch", "name", "protected", "rules", "command", "allow_failure"];

  // podSubtitle is the second line of a dev pod, command or stop tile.
  function podSubtitle(s) {
    if (s.service === "devpod") return s.repository ? `${s.repository} · ${s.work_branch || DEFAULT_WORK_BRANCH}` : "No repository";
    if (s.service === "command") return String(s.command || "").trim() || "No command";
    return "Deletes the dev pod";
  }

  // linesOf reads a field with one entry per line: trimmed, without the empty lines.
  const linesOf = (typed) => String(typed ?? "").split("\n").map((line) => line.trim()).filter(Boolean);

  // ruleWith returns a rule with one field changed, as it is stored: tool and message always,
  // path and matches only when they hold something.
  function ruleWith(rule, key, value) {
    const next = { ...rule, [key]: String(value ?? "").trim() };
    const out = { tool: RULE_TOOLS.includes(next.tool) ? next.tool : RULE_TOOLS[0] };
    for (const part of ["path", "matches"]) if (next[part]) out[part] = next[part];
    out.message = next.message || "";
    return out;
  }

  // dropForeignFields removes the fields that do not belong to the step's kind: a merge or
  // batch keeps only its parameters, a dev pod, command or stop step only its own fields, and
  // any other kind loses the fields only those kinds have.
  function dropForeignFields(s) {
    const own = isNode(s) ? Object.keys(NODE_PARAMS[s.service]) : isPodKind(s) ? POD_FIELDS[s.service] : null;
    for (const key of Object.keys(s)) {
      if (KEPT_FIELDS.includes(key)) continue;
      if (own ? !own.includes(key) : key in PARAM_NAME || POD_ONLY.includes(key)) delete s[key];
    }
    return s;
  }

  // paramValue reads a typed parameter: "" leaves it out, a number is stored as typed (also a
  // wrong one, so its problem shows), and anything else is left out.
  function paramValue(typed) {
    const text = String(typed ?? "").trim();
    return text !== "" && Number.isFinite(Number(text)) ? Number(text) : undefined;
  }

  // stepProblems: why the orchestrator would refuse this step, one sentence each; empty when
  // fine. It covers merge and batch nodes, a route step that follows one, the dev pod steps,
  // and a gate's back edge. name turns a step ID into what the page shows.
  function stepProblems(w, s, name = (id) => id) {
    const problems = [];
    const deps = s.depends_on || [];
    const whole = (value) => Number.isInteger(value) && value >= 1;
    const byId = stepsById(w);
    // The steps this one comes after, directly or through others.
    const before = () => [...new Set(deps.flatMap((dep) => [dep, ...ancestors(w, dep)]))].map((id) => byId.get(id)).filter(Boolean);
    if (s.service === "devpod") {
      const source = String(s.source_branch || "").trim(), work = String(s.work_branch || "").trim();
      if (!s.repository) problems.push("A dev pod needs a repository");
      if (!source) problems.push("A dev pod needs a source branch");
      if (!work) problems.push("A dev pod needs a work branch");
      else if (work === source) problems.push("The work branch must differ from the source branch");
      const other = (w?.steps || []).find((o) => o !== s && o.id !== s.id && o.service === "devpod");
      if (other) problems.push(`A workflow has only one dev pod step, and ${name(other.id)} is another`);
      const node = before().find(isNode);
      if (node) problems.push(`A dev pod cannot come after the ${node.service} step ${name(node.id)}`);
      (Array.isArray(s.rules) ? s.rules : []).forEach((rule, i) => {
        if (!RULE_TOOLS.includes(rule?.tool)) problems.push(`Rule ${i + 1} needs a tool`);
        if (!String(rule?.message || "").trim()) problems.push(`Rule ${i + 1} needs a message that names the rule, the reason and the fix`);
        try { new RegExp(rule?.matches || ""); } catch { problems.push(`Rule ${i + 1} has a pattern that is not a regular expression`); }
      });
    }
    if (s.service === "command" || s.service === "devpod-stop") {
      if (s.service === "command" && !String(s.command || "").trim()) problems.push("A command step needs a command");
      if (!before().some((o) => o.service === "devpod")) problems.push(`A ${s.service === "command" ? "command" : "stop"} step must come after the dev pod step`);
    }
    if (s.service === "gate") {
      if (s.on_reject) { const refusal = rejectRefusal(w, s, s.on_reject, name); if (refusal) problems.push(refusal); }
      if (s.max_rejections !== undefined && s.max_rejections !== null && s.max_rejections !== "" && !whole(s.max_rejections)) problems.push("Max rejections must be a whole number of 1 or more");
    }
    if (isNode(s)) {
      const word = s.service === "merge" ? "A merge" : "A batch";
      if (!deps.length) problems.push(`${word} needs at least one source`);
      if (s.service === "batch" && (s.size === undefined || s.size === null || s.size === "")) problems.push("A batch needs a size");
      for (const key of Object.keys(NODE_PARAMS[s.service])) {
        const value = s[key];
        if (value === undefined || value === null || value === "") continue;
        if (!Number.isInteger(value) || value < 1) problems.push(`${PARAM_NAME[key]} must be a whole number of 1 or more`);
      }
    }
    if (s.service === "route") {
      for (const id of deps) {
        const from = (w?.steps || []).find((o) => o.id === id);
        if (isNode(from)) problems.push(`A router cannot follow a ${from.service}: ${id}`);
      }
    }
    return problems;
  }

  // tileOutline is the outline of a step's tile by kind, as a path in a box of w by h: an agent a
  // rounded rectangle, a gate a rectangle with cut corners (a stop sign: a person decides), an
  // interactive session a pill (a speech bubble), a router a signpost pointing on, a workflow a
  // square-cornered box for its side bands, a merge a funnel (wide at the top, narrow at the
  // bottom: many results become one message), a batch a tray (a plain box; the page draws a
  // second line under its top edge), a dev pod a hexagon (a pod: a point on each side), a
  // command a slanted box (the flowchart sign for a manual step: it runs without a model), a
  // stop step a box notched on each side (the pod's points turned inwards: it is taken away),
  // a team and anything else the rounded rectangle.
  const FUNNEL_NECK = 28, FUNNEL_RIM = 20; // how far the funnel narrows on each side, and where it starts to
  const POD_POINT = 14, COMMAND_SLANT = 12;
  function tileOutline(kindName, w, h) {
    if (kindName === "devpod") { const p = POD_POINT; return `M${p},0 H${w - p} L${w},${h / 2} L${w - p},${h} H${p} L0,${h / 2} z`; }
    if (kindName === "devpod-stop") { const p = POD_POINT; return `M0,0 H${w} L${w - p},${h / 2} L${w},${h} H0 L${p},${h / 2} z`; }
    if (kindName === "command") { const c = COMMAND_SLANT; return `M${c},0 H${w} L${w - c},${h} H0 z`; }
    const round = (r) => `M${r},0 H${w - r} A${r},${r} 0 0 1 ${w},${r} V${h - r} A${r},${r} 0 0 1 ${w - r},${h} H${r} A${r},${r} 0 0 1 0,${h - r} V${r} A${r},${r} 0 0 1 ${r},0 z`;
    if (kindName === "gate") { const c = 14; return `M${c},0 H${w - c} L${w},${c} V${h - c} L${w - c},${h} H${c} L0,${h - c} V${c} z`; }
    if (kindName === "interactive") return round(h / 2);
    if (kindName === "route") { const p = 18; return `M0,0 H${w - p} L${w},${h / 2} L${w - p},${h} H0 z`; }
    if (kindName === "workflow") return round(2);
    if (kindName === "merge") return `M0,0 H${w} V${FUNNEL_RIM} L${w - FUNNEL_NECK},${h} H${FUNNEL_NECK} L0,${FUNNEL_RIM} z`;
    if (kindName === "batch") return round(2);
    return round(8);
  }

  return {
    NODE_KINDS, isNode, NODE_PARAMS, nodeSubtitle, dropForeignFields, paramValue, stepProblems, tileOutline,
    POD_KINDS, isPodKind, DEFAULT_WORK_BRANCH, RULE_TOOLS, podSubtitle, linesOf, ruleWith,
    REJECT_KINDS, ancestors, dependents, rejectRefusal, backEdgeTip, backEdgeRoute, REJECT_PORT_Y,
    RESERVED_MEMBER, memberName, teamMembers, channelId, channelProblem, defaultChannels, teamChannels, toggleChannel, pruneChannels, MEMBER_H, ONE_WAY, channelSender, setChannelDirection,
    teamProblems, memberTip, setLead, removeMember, nestedRefs, nestedNames, reachesWorkflow, nestableWorkflows, renameWorkflowRefs,
    newestRef, versionWord, versionOptions, storable, fingerprint, headState, runBlocker,
    rejectTargets, parseHash, formatHash, teamLayout, edgeRoute,
  };
})();

// ---- Page ----

(() => {
  const API = "/api/orchestrator";
  const SVG_NS = "http://www.w3.org/2000/svg";
  const TILE_W = 220, TILE_H = 64;
  const BAND_W = 12; // the side bands of a workflow node
  const TOP = 40, ROW_H = TILE_H + 72, COL_W = TILE_W + 48, GRAPH_MARGIN = 32;
  const L = WorkflowLogic;
  const SPECIAL = ["agent", "gate", "interactive", "route", "workflow", "team", "merge", "batch", ...L.POD_KINDS];
  const TYPE_ICON = { agent: "agent", gate: "gate", interactive: "chat", route: "workflow", workflow: "workflow" };
  const TYPE_NAME = { agent: "Agent", gate: "Human Gate", interactive: "Interactive Session", route: "Router", workflow: "Workflow", team: "Team", merge: "Merge", batch: "Batch", devpod: "Dev Pod", "devpod-stop": "Stop Dev Pod", command: "Command" };
  // A new step is named by its kind and the lowest free number: Step 1, Dev Pod 1, Stop Dev Pod 1.
  const STEP_WORD = { agent: "step", devpod: "dev-pod", "devpod-stop": "stop-dev-pod" };
  // A team's tile icon: three members joined by channels.
  const TEAM_ICON = '<circle cx="8" cy="4" r="2"/><circle cx="3.5" cy="12" r="2"/><circle cx="12.5" cy="12" r="2"/><path d="M7 5.8L4.6 10.2M9 5.8l2.4 4.4M5.5 12h5"/>';
  // A merge's tile icon: two lines that join into one. A batch's: three results stacked in a tray.
  const MERGE_ICON = '<path d="M3 2.5l5 5.5v5.5M13 2.5L8 8"/>';
  const BATCH_ICON = '<path d="M2 9.5V12a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V9.5"/><path d="M5 10h6M5 6.5h6M5 3h6"/>';
  // A dev pod's tile icon: a box. A command's: a shell prompt. A stop step's: the box crossed out.
  const DEVPOD_ICON = '<path d="M8 2l5 2.8v6.4L8 14l-5-2.8V4.8z"/><path d="M3 4.8l5 2.8 5-2.8M8 7.6V14"/>';
  const COMMAND_ICON = '<path d="M3 4.5L6.5 8 3 11.5M8.5 12H13"/>';
  const DEVPOD_STOP_ICON = '<rect x="3" y="3" width="10" height="10" rx="1.5"/><path d="M6 6l4 4M10 6l-4 4"/>';
  const KIND_ICON = { team: TEAM_ICON, merge: MERGE_ICON, batch: BATCH_ICON, devpod: DEVPOD_ICON, command: COMMAND_ICON, "devpod-stop": DEVPOD_STOP_ICON };
  // An agent step runs one agent; an interactive step lets a person chat with its main agent
  // (agent) and any further ones (agents).
  const usesAgent = (s) => s.service === "agent" || s.service === "interactive";
  const typeName = (s) => TYPE_NAME[s.service] || "Service";

  const state = {
    workflows: [], // the newest stored version of every workflow, and a new one not saved yet
    current: -1, // the index of the workflow on the canvas
    edit: null, // the copy on the canvas: a stored version with the edits made since
    baseline: "", // the fingerprint of that version as stored
    selected: null, dirty: false, saving: false, loaded: false,
    loadError: "", stale: true, agentRefs: [], agentsLoaded: false, inspectorOpen: true,
    repos: [], reposLoaded: false, reposError: "", // the Git repositories of the Connectors page, for a dev pod step
    path: [], // references of the workflows the current one was opened through, root first
    teamOpen: null, // { workflow, step }: the team node opened on the canvas
  };
  // The node panel is open unless its toggle closed it; a click on a node opens it again.
  state.inspectorOpen = true;
  try { state.inspectorOpen = localStorage.getItem("factory.inspector") !== "closed"; } catch { /* storage unavailable */ }
  let drag = null;
  let descriptionEdit = null;
  const descriptionDirty = () => !!descriptionEdit && descriptionEdit.value !== descriptionEdit.original;
  const descriptionActionBlocked = (action) => {
    if (!descriptionEdit) return false;
    setStatus(`Not ${action}: the description is being edited`, "error");
    return true;
  };

  // workflow is what the canvas shows and every edit changes; entry is its row in the list.
  const workflow = () => state.edit;
  const entry = () => state.workflows[state.current];
  const step = (id) => workflow()?.steps.find((s) => s.id === id);
  // A workflow without a stored version is a draft: New workflow makes one, its first Save stores v1.
  const isDraft = (w) => !!w && !WorkflowRef.newest(w);
  const stored = () => state.workflows.filter((w) => !isDraft(w));
  const newestOf = (name) => WorkflowRef.newest(state.workflows.find((w) => w.name === name));
  // shownVersion is the stored version on the canvas; 0 for a draft.
  const shownVersion = () => (state.edit && !isDraft(entry()) ? Number(state.edit.version) || WorkflowRef.newest(entry()) : 0);
  // here is the reference of what the canvas shows, pinned only when it is not the newest version.
  const here = () => WorkflowRef.format(workflow().name, shownVersion() !== WorkflowRef.newest(entry()) ? shownVersion() : 0);
  const refLabel = (ref) => `${titleCase(WorkflowRef.nameOf(ref))}${L.versionWord(ref) ? ` ${L.versionWord(ref)}` : ""}`;
  const pinNewest = (name) => L.newestRef(stored(), name);
  // nestable: the stored workflows the one on the canvas may start without reaching itself.
  const nestable = () => L.nestableWorkflows(stored(), workflow().name);
  const inPort = (s) => ({ x: s.position.x + TILE_W / 2, y: s.position.y });
  const outPort = (s) => ({ x: s.position.x + TILE_W / 2, y: s.position.y + TILE_H });
  const kind = (s) => (SPECIAL.includes(s.service) ? s.service : "custom");

  // ---- Save: edits stay on the page until Save stores them as the next version. ----

  // showDefinition puts a stored version (or a draft) on the canvas as the copy that edits change.
  function showDefinition(def) {
    descriptionEdit = null;
    state.edit = def ? structuredClone(def) : null;
    if (state.edit) {
      if (!Array.isArray(state.edit.steps)) state.edit.steps = [];
      autoLayout(state.edit);
    }
    state.baseline = state.edit ? L.fingerprint(state.edit) : "";
    refreshDirty();
  }

  // refreshDirty compares the canvas with the stored version. A draft has unsaved work once it has a step.
  function refreshDirty() {
    const w = workflow();
    state.dirty = !!w && (isDraft(entry()) ? w.steps.length > 0 || !!w.description : L.fingerprint(w) !== state.baseline);
  }

  function markDirty() {
    dropUndo(); // Cmd/Ctrl+Z never undoes more than the last edit
    refreshDirty();
    renderHead();
  }

  const headState = () => L.headState({
    draft: isDraft(entry()), version: shownVersion(), newest: WorkflowRef.newest(entry()),
    dirty: state.dirty, steps: workflow()?.steps.length || 0, saving: state.saving,
  });

  // The orchestrator quotes IDs; they read as names, like everywhere else.
  const namedError = (e) => e.message.replace(/"([^"]+)"/g, (_, id) => titleCase(id));

  // withVersions completes a definition the orchestrator returned: a stored workflow without a
  // version is v1, and its list of versions holds at least its own.
  function withVersions(def, known = []) {
    const version = Number(def.version) > 0 ? Number(def.version) : 1;
    const listed = Array.isArray(def.versions) && def.versions.length ? def.versions : [...known, version];
    return { ...def, version, versions: [...new Set(listed.map(Number))].sort((a, b) => a - b), steps: Array.isArray(def.steps) ? def.steps : [] };
  }

  // save stores the workflow on the canvas as its next version and shows that version.
  async function save() {
    const w = workflow(), e = entry();
    if (!w || !e || !headState().canSave || descriptionEdit) return;
    const sent = L.fingerprint(w);
    state.saving = true;
    renderHead();
    try {
      const answer = await api("POST", `${API}/workflows/${encodeURIComponent(w.name)}/versions`, L.storable(w), creatorHeaders());
      const def = withVersions(answer, e.versions || []);
      const at = state.workflows.indexOf(e);
      if (at >= 0) state.workflows[at] = def;
      versionCache.clear();
      if (workflow() === w) {
        // Edits made while the save was on its way stay, as unsaved edits of the new version.
        if (L.fingerprint(w) === sent) showDefinition(def);
        else { w.version = def.version; w.versions = def.versions; state.baseline = sent; }
      }
      setSaveState(`Saved v${def.version}`);
    } catch (err) {
      setStatus(`Not saved: ${namedError(err)}`, "error");
    } finally {
      state.saving = false;
    }
    render();
  }

  // confirmLeave asks before unsaved edits are discarded, as on Agents and Policies.
  const confirmLeave = async () => descriptionEdit?.saving ? false : !(state.dirty || descriptionDirty()) || confirmAction("Discard your unsaved changes?", "Discard");

  // runBlocker explains why runs cannot start now, or returns "".
  const runBlocker = () => L.runBlocker({ draft: isDraft(entry()), dirty: state.dirty, steps: workflow()?.steps.length || 0, saving: state.saving });
  Inputs.setBlocker(runBlocker);

  // openInputs opens Select Inputs for the version on the canvas. With unsaved edits it is
  // refused, since a run uses a stored version.
  function openInputs() {
    const blocked = runBlocker();
    if (blocked) { setStatus(`Not started: ${blocked[0].toLowerCase()}${blocked.slice(1)}`, "error"); return; }
    Inputs.open(workflow().name, shownVersion());
  }

  // The stored versions other than the newest, loaded when they are opened or looked at.
  const versionCache = new Map(); // "name@vN" -> { status: "loading" | "ok" | "error", def, error, promise }
  function versionEntry(name, version) {
    const ref = WorkflowRef.format(name, version);
    let found = versionCache.get(ref);
    if (!found) {
      found = { status: "loading" };
      const mine = found;
      found.promise = api("GET", `${API}/workflows/${encodeURIComponent(name)}/versions/${version}`).then(
        (def) => { mine.status = "ok"; mine.def = withVersions({ ...def, version: Number(def.version) || version }); return mine.def; },
        (err) => { mine.status = "error"; mine.error = err.message; throw err; });
      found.promise.catch(() => {}); // reported where the version is asked for
      versionCache.set(ref, found);
    }
    return found;
  }

  // pinnedDef is the definition a reference names, for the node panel: the newest from the list,
  // an older version once it is loaded (the panel is drawn again when it arrives).
  function pinnedDef(ref) {
    if (missingWorkflow(ref)) return { status: "missing" };
    const def = WorkflowRef.find(stored(), ref);
    if (def) return { status: "ok", def };
    const { name, version } = WorkflowRef.parse(ref);
    const found = versionEntry(name, version);
    if (found.status === "loading" && !found.redraw) { found.redraw = true; found.promise.then(refreshAfterEdit, refreshAfterEdit); }
    return found;
  }

  // ---- Loading ----

  // load reads the newest version of every workflow, then opens what the hash names.
  let wanted = { rest: "", remembered: false }; // the hash to follow once the workflows are loaded
  async function load() {
    try {
      const cfg = await api("GET", `${API}/workflows`);
      const before = entry()?.name;
      state.workflows = (cfg.workflows || []).map((w) => withVersions(w));
      versionCache.clear();
      state.current = state.workflows.findIndex((w) => w.name === before);
      if (state.current < 0) resetView();
      state.edit = null;
      state.selected = null;
      state.dirty = false;
      state.loaded = true;
      state.loadError = "";
      dropUndo();
      await followHash(wanted.rest, wanted.remembered);
    } catch (e) {
      state.loadError = `Cannot load the workflows: ${e.message}`;
      render();
    }
  }

  // loadAgents fills the agent choices with every version of every agent, latest first.
  async function loadAgents() {
    try {
      const data = await api("GET", "/api/agents/agents");
      state.agentRefs = data.entries.flatMap((e) => [...e.versions].reverse().map((v) => `${e.name}@${v}`));
      state.agentsLoaded = true;
      renderInspector();
      renderCanvas(); // steps whose agent is gone turn red
    } catch (e) { setStatus(`Cannot load agents: ${e.message}`, "error"); }
  }

  // loadRepositories fills the Repository choice of a dev pod step from the connector service.
  // A failure stays quiet: the field says why it offers nothing.
  async function loadRepositories() {
    try {
      state.repos = ConnectorsLogic.repositories(await api("GET", "/api/connectors/git/repositories"));
      state.reposLoaded = true;
      state.reposError = "";
    } catch (e) { state.reposError = `Cannot list the repositories: ${e.message}`; }
    renderInspector();
  }

  // ---- Layout ----

  // autoLayout gives steps without a position a row per dependency depth; siblings sit side by side.
  function autoLayout(w) {
    const depth = {};
    const depthOf = (s, seen = new Set()) => {
      if (depth[s.id] !== undefined) return depth[s.id];
      if (seen.has(s.id)) return 0;
      seen.add(s.id);
      const deps = (s.depends_on || []).map((d) => w.steps.find((x) => x.id === d)).filter(Boolean);
      return (depth[s.id] = deps.length ? 1 + Math.max(...deps.map((d) => depthOf(d, seen))) : 0);
    };
    for (const s of w.steps) if (!s.position) s.position = freeSlot(w, depthOf(s));
  }

  // freeSlot returns the leftmost position in a row that no positioned step overlaps.
  function freeSlot(w, row) {
    const y = TOP + row * ROW_H;
    for (let col = 0; ; col++) {
      const x = col * COL_W;
      const busy = w.steps.some((o) => o.position &&
        o.position.x < x + TILE_W + 24 && o.position.x + TILE_W + 24 > x &&
        o.position.y < y + TILE_H + 24 && o.position.y + TILE_H + 24 > y);
      if (!busy) return { x, y };
    }
  }

  function el(name, attrs = {}, parent) {
    const e = document.createElementNS(SVG_NS, name);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    if (parent) parent.appendChild(e);
    return e;
  }

  // edgePath draws a dependency from an output port (bottom) to an input port (top). It ends
  // 14 px above the port centre, so the arrowhead (about 8 px long) touches the port circle
  // (radius 5 plus its stroke) instead of hiding under it.
  // edgePath draws a dependency from an output port down to an input port. An edge that skips a
  // row would run behind the tiles in between, so it turns aside in the gap under its source, runs
  // down the free channel beside the source column (toward its target; for the same column, left
  // of the leftmost column, else right), and turns back in the gap above its target. Edges that
  // skip more rows run a little further out, so parallel edges stay apart and no line crosses a tile.
  function edgePath(a, b, left = false) {
    const sx = laneX(a, b, left);
    if (sx !== null) {
      const gap = ROW_H - TILE_H;
      return `M${a.x},${a.y} C${a.x},${a.y + gap * 0.7} ${sx},${a.y + gap * 0.2} ${sx},${a.y + gap * 0.85}`
        + ` L${sx},${b.y - gap * 0.85} C${sx},${b.y - gap * 0.2} ${b.x},${b.y - gap * 0.7} ${b.x},${b.y - 14}`;
    }
    const dy = Math.max(30, Math.abs(b.y - a.y) / 2);
    return `M${a.x},${a.y} C${a.x},${a.y + dy} ${b.x},${b.y - dy} ${b.x},${b.y - 14}`;
  }

  // dependencyPath draws the arrow from a step to one that depends on it: down from port to
  // port, or, for a step beside its source, from side to side (see WorkflowLogic.edgeRoute), so
  // that no arrow runs behind a tile. It ends 9 px before the tile, where the arrowhead touches it.
  function dependencyPath(w, from, s) {
    const route = L.edgeRoute(from.position, s.position, w.steps.filter((o) => o !== from && o !== s && o.position).map((o) => o.position), { w: TILE_W, h: TILE_H });
    if (route.kind === "down") return edgePath(outPort(from), inPort(s), bowsLeft(w, from, s));
    const d = route.dir;
    const a = { x: from.position.x + (d > 0 ? TILE_W : 0), y: from.position.y + TILE_H / 2 };
    const b = { x: s.position.x + (d > 0 ? 0 : TILE_W), y: s.position.y + TILE_H / 2 };
    if (route.kind === "side") {
      const dx = Math.max(16, Math.abs(b.x - a.x) / 2);
      return `M${a.x},${a.y} C${a.x + d * dx},${a.y} ${b.x - d * dx},${b.y} ${b.x - d * 9},${b.y}`;
    }
    // Under the tiles in between: down the channel beside the source, along the gap under the row, up the channel beside the target.
    // The channels sit in the middle of the 20 px gap between two columns, and the arrow leaves and
    // enters below the tiles' middle, so it does not lie on a side-to-side arrow of the same tile.
    const xa = a.x + d * 10, xb = b.x - d * 10, y = route.y, r = 8, ya = a.y + 14, yb = b.y + 14;
    return `M${a.x},${ya} Q${xa},${ya} ${xa},${ya + r} L${xa},${y - r} Q${xa},${y} ${xa + d * r},${y}`
      + ` L${xb - d * r},${y} Q${xb},${y} ${xb},${y - r} L${xb},${yb + r} Q${xb},${yb} ${b.x - d * 9},${yb}`;
  }

  // laneX is the x of the lane an arrow from port a to port b runs down, or null when it skips no row.
  function laneX(a, b, left) {
    const rows = Math.round((b.y - a.y) / ROW_H);
    if (rows < 2) return null;
    const spread = Math.min(rows - 2, 4) * 6; // stays inside the 48 px channel between columns
    return b.x > a.x + TILE_W / 2 ? a.x + TILE_W / 2 + 12 + spread
      : b.x < a.x - TILE_W / 2 ? a.x - TILE_W / 2 - 12 - spread
        : left ? Math.min(a.x, b.x) - TILE_W / 2 - 28 - spread * 2 : Math.max(a.x, b.x) + TILE_W / 2 + 12 + spread;
  }

  // bowsLeft: an arrow within the leftmost column takes the lane left of it.
  const bowsLeft = (w, from, s) => Math.min(from.position.x, s.position.x) <= Math.min(...w.steps.map((x) => x.position.x)) + 1;

  // missingRef: the first agent version a step is pinned to that the registry no longer has, or "".
  // missingWorkflow: a reference to a workflow that is not stored, or to a version it does not have.
  const missingWorkflow = (ref) => {
    if (!ref || !state.loaded) return false;
    const { name, version } = WorkflowRef.parse(ref);
    const def = stored().find((w) => w.name === name);
    return !def || (version > 0 && !(def.versions || []).map(Number).includes(version));
  };
  const missingRef = (s) => {
    // A workflow step or a team names workflows and agents that can be gone as well.
    if (s.service === "workflow") return missingWorkflow(s.workflow) ? s.workflow : "";
    if (s.service === "team") {
      return (state.agentsLoaded && [s.agent, ...(s.agents || [])].find((ref) => ref && !state.agentRefs.includes(ref)))
        || (s.workflows || []).find(missingWorkflow) || "";
    }
    return !usesAgent(s) || !state.agentsLoaded ? ""
      : [s.agent, ...(s.service === "interactive" ? s.agents || [] : [])].find((ref) => ref && !state.agentRefs.includes(ref)) || "";
  };
  const missingAgent = (s) => !!missingRef(s);

  function subtitle(s) {
    switch (s.service) {
      case "agent": case "interactive": return missingAgent(s) ? `Missing: ${missingRef(s)}` : s.agent || "";
      case "gate": return s.question || "";
      case "route": return "";
      case "merge": case "batch": return L.nodeSubtitle(s);
      case "devpod": case "command": case "devpod-stop": return L.podSubtitle(s);
      case "workflow": return missingAgent(s) ? `Missing: ${refLabel(s.workflow)}` : refLabel(s.workflow || ""); // the workflow and its pinned version
      case "team": {
        if (missingAgent(s)) return `Missing: ${missingRef(s)}`;
        const count = L.teamMembers(s).length;
        return `${s.agent || "No lead agent"} · ${count} member${count === 1 ? "" : "s"}`;
      }
      default: return `${s.service || "?"} · ${s.activity || "Process"}`;
    }
  }

  const fit = (text, width, px = 6.6) => {
    const max = Math.max(4, Math.floor(width / px));
    return text.length > max ? `${text.slice(0, max - 1)}…` : text;
  };

  // ---- Rendering ----

  // firstStep is the step at the top of the graph (then the leftmost).
  const firstStep = (w) => [...w.steps].sort((a, b) => a.position.y - b.position.y || a.position.x - b.position.x)[0];

  function render() {
    const w = workflow();
    if (w && w.steps.length && (!state.selected || !step(state.selected))) state.selected = firstStep(w).id;
    // A back edge belongs to a gate and leads to a step that exists; one the orchestrator would
    // refuse for another reason stays and shows as a problem in the gate's panel.
    for (const s of w?.steps || []) {
      if (s.on_reject && !(s.service === "gate" && step(s.on_reject))) delete s.on_reject;
      if (!s.on_reject && s.max_rejections !== undefined) delete s.max_rejections;
    }
    // An opened team closes when its workflow or its step is gone from the canvas.
    if (state.teamOpen && (state.teamOpen.workflow !== w?.name || step(state.teamOpen.step)?.service !== "team")) state.teamOpen = null;
    refreshDirty();
    renderPath();
    renderHead();
    renderList();
    renderDescription();
    renderInspector();
    renderCanvas();
    renderToggles();
    syncHash();
  }

  // ---- Sidebar: a tree of groups and their workflows, as on Agents and Policies ----

  // Tree ids: "g:<group>" for a group, "w:<name>" for a workflow.
  const groups = new Set(); // groups made with New group… in this visit; stored once they hold a workflow
  const groupList = () => groupPaths(state.workflows.map((w) => w.name), groups);
  const indexOf = (name) => state.workflows.findIndex((w) => w.name === name);
  const tree = createTree($("wf-list"), {
    // A click on a workflow opens it; when unsaved edits are kept instead, the tree marks the one on the canvas again.
    onSelect(n) { if (n.id.startsWith("w:")) openTarget({ name: n.id.slice(2) }).then((shown) => { if (!shown) showSelection(); }); },
    onContext(n, ev) { if (n.id.startsWith("g:")) groupMenu(n.id.slice(2), ev); else workflowMenu(n.id.slice(2), ev); },
  });
  let treeShows = null; // the workflow the tree last marked as the one on the canvas

  // treeOrder lists the workflow names in the order the tree shows them.
  const treeOrder = () => groupNodes(groupList(), state.workflows, (w) => w.name, (_g, children) => children, (w) => w.name).flat(Infinity);

  function renderList() {
    if (state.loadError || !state.loaded) { tree.setRoots([]); treeShows = null; return; } // the canvas shows a load error once
    const label = (path) => titleCase(leafOf(path));
    tree.setRoots(groupNodes(groupList(), state.workflows, (w) => w.name,
      (g, children) => ({ id: `g:${g}`, label: label(g), full: label(g), children }),
      (w) => ({ id: `w:${w.name}`, label: label(w.name), full: label(w.name) })));
    showSelection(false);
  }

  // showSelection marks the workflow on the canvas in the tree and opens the groups that hold it.
  // Unforced, it does so once per workflow shown, so a group picked since keeps the selection.
  function showSelection(force = true) {
    const name = entry()?.name || null;
    if (!force && name === treeShows) return;
    treeShows = name;
    tree.select(name ? `w:${name}` : null);
    if (name) tree.reveal(groupIds(name, "g:"));
  }

  // newItems are the New actions inside a group ("" is the top level); every menu ends with them.
  const newItems = (group) => [
    ["New workflow", () => newWorkflow(group), "", group ? `Adds an empty workflow to ${titleCase(group)}` : "Adds an empty workflow"],
    ["New group…", () => newGroup(group), "", group ? `Adds a group inside ${titleCase(group)}` : "Adds a group"],
  ];

  // Right-click acts on a workflow without opening it, like every other sidebar, grouped by
  // scope: this workflow, then its Delete, then New last.
  function workflowMenu(name, ev) {
    const shown = titleCase(name);
    openMenu(ev, [
      ["Rename workflow…", () => renameWorkflow(indexOf(name)), "", "Renames it with every version, or moves it into another group"],
      ["Duplicate workflow", () => duplicateWorkflow(indexOf(name)), "", `Copies ${shown} with all steps and positions`],
      "-",
      ["Delete workflow", () => deleteWorkflow(indexOf(name)), "danger", `Deletes ${shown} with all its versions`],
      "-",
      ...newItems(groupOf(name)),
    ]);
  }

  function groupMenu(group, ev) {
    const count = state.workflows.filter((w) => w.name.startsWith(`${group}/`)).length;
    openMenu(ev, [
      ["Delete group", () => deleteGroup(group), "danger", count ? `Deletes ${titleCase(group)} with its ${count} workflow${count === 1 ? "" : "s"}` : `Deletes ${titleCase(group)}`],
      "-",
      ...newItems(group),
    ]);
  }

  // openTarget shows a workflow version on the canvas: { name, version, step, path }; version 0
  // is the newest. Leaving unsaved edits asks first. It resolves false when the edits are kept
  // or there is no such workflow.
  let openSeq = 0;
  async function openTarget({ name, version = 0, step: stepId = "", path = [] }) {
    const e = state.workflows.find((w) => w.name === name);
    if (!e) { setStatus(`There is no workflow named ${titleCase(name)}`, "error"); return false; }
    const newest = WorkflowRef.newest(e);
    // A version that is gone gives way to the newest, like every linked item that no longer exists.
    const want = version && (e.versions || []).map(Number).includes(version) ? version : newest;
    const same = e === entry() && !!state.edit && want === shownVersion();
    if (!same) {
      if (!(await confirmLeave())) return false;
      const seq = ++openSeq;
      let def = e;
      if (want !== newest) {
        const ref = WorkflowRef.format(name, want);
        if (versionCache.get(ref)?.status === "error") versionCache.delete(ref); // asked again
        try { def = await versionEntry(name, want).promise; } catch (err) { setStatus(`Cannot load v${want} of ${titleCase(name)}: ${err.message}`, "error"); }
        if (seq !== openSeq) return false;
      }
      // The list may have been loaded again while the question or the version was awaited, so the
      // workflow is looked up by name again; one that is gone is said, never dropped silently.
      const target = state.workflows.includes(e) ? e : state.workflows.find((w) => w.name === name);
      if (!target) { setStatus(`There is no workflow named ${titleCase(name)}`, "error"); return false; }
      if (def === e) def = target;
      // A draft that is left is gone: it was never stored.
      const left = entry();
      if (left && left !== target && isDraft(left)) state.workflows.splice(state.workflows.indexOf(left), 1);
      const index = state.workflows.indexOf(target);
      if (index < 0) { setStatus(`There is no workflow named ${titleCase(name)}`, "error"); return false; }
      if (index !== state.current) resetView();
      state.current = index;
      showDefinition(def);
      dropUndo();
    }
    // The way a nested workflow was reached comes along, so reload, Back, and Forward keep it.
    state.path = path.filter((ref) => WorkflowRef.nameOf(ref) !== name && stored().some((w) => w.name === WorkflowRef.nameOf(ref)));
    state.selected = stepId && state.edit.steps.some((x) => x.id === stepId) ? stepId : null;
    render();
    return true;
  }

  // openNested opens the workflow version a workflow node or a team member runs and remembers the way there.
  function openNested(ref) {
    const from = workflow();
    const { name, version } = WorkflowRef.parse(ref);
    if (!from || !name) return;
    if (missingWorkflow(ref)) { setStatus(`${refLabel(ref)} no longer exists`, "error"); return; }
    if (name === from.name) return;
    openTarget({ name, version, path: [...state.path, here()] });
  }

  // The path line sits above the canvas while the workflow was opened through a workflow node:
  // root › nested › current. Every part before the current one links back to that workflow.
  const pathLine = h("nav", { className: "wf-path", "aria-label": "Nested workflow path", hidden: true });
  document.querySelector("#tab-workflows .canvas-head").prepend(pathLine);

  function renderPath() {
    const w = workflow();
    state.path = w ? state.path.filter((ref) => WorkflowRef.nameOf(ref) !== w.name && state.workflows.some((x) => x.name === WorkflowRef.nameOf(ref))) : [];
    const team = state.teamOpen ? step(state.teamOpen.step) : null;
    pathLine.hidden = !state.path.length && !team;
    if (pathLine.hidden) { pathLine.replaceChildren(); return; }
    const parts = [];
    state.path.forEach((ref, i) => {
      // A part that is not the newest version of its workflow names its version.
      const { name, version } = WorkflowRef.parse(ref);
      const older = version && version !== newestOf(name) ? version : 0;
      parts.push(h("a", { href: L.formatHash({ workflow: name, version: older, path: state.path.slice(0, i) }) }, older ? refLabel(ref) : titleCase(name)),
        h("span", { className: "wf-path-sep", "aria-hidden": "true" }, "›"));
    });
    // With a team open, the workflow is a link back to its graph and the team is the current part.
    if (team) {
      const back = h("a", { href: currentHash(), title: "Back to the workflow graph" }, titleCase(w.name));
      back.addEventListener("click", (ev) => { ev.preventDefault(); state.teamOpen = null; render(); });
      parts.push(back, h("span", { className: "wf-path-sep", "aria-hidden": "true" }, "›"),
        h("span", { className: "wf-path-current", "aria-current": "page" }, `${titleCase(team.id)} (team)`));
    } else parts.push(h("span", { className: "wf-path-current", "aria-current": "page" }, titleCase(w.name)));
    pathLine.replaceChildren(...parts);
  }

  // The canvas head: the version on the canvas, a note for an older version, and Save, the
  // page's one primary button.
  const versionSelect = h("select", { className: "fit wf-version", "aria-label": "Version" });
  const versionNote = h("span", { className: "wf-version-note", role: "status" });
  const saveButton = h("button", { type: "button", className: "primary wf-save", onclick: () => save() }, "Save");
  document.querySelector("#tab-workflows .canvas-head").append(versionSelect, versionNote, saveButton);
  versionSelect.addEventListener("change", async () => {
    const e = entry();
    if (!e) return;
    const shown = await openTarget({ name: e.name, version: Number(versionSelect.value), step: state.selected || "", path: state.path });
    if (!shown) renderHead(); // the edits stay: the select names the version on the canvas again
  });

  function renderHead() {
    const w = workflow(), e = entry();
    const head = headState();
    versionSelect.hidden = !w || isDraft(e);
    if (!versionSelect.hidden) {
      versionSelect.replaceChildren(...L.versionOptions(e.versions).map((o) => h("option", { value: String(o.version), selected: o.version === shownVersion() }, o.label)));
      versionSelect.title = "Shows another stored version of this workflow";
    }
    versionNote.hidden = !w || !head.note;
    versionNote.textContent = w ? head.note : "";
    saveButton.hidden = !w;
    saveButton.disabled = !head.canSave || !!descriptionEdit;
    saveButton.title = descriptionEdit?.saving ? "Stores the description now" : descriptionEdit ? "Needs the description saved or cancelled first" : head.saveTip;
  }

  // The selected version's description is editable independently of its execution graph.
  const singleLineDescription = (value) => String(value || "").replace(/\s+/g, " ");
  function growDescription(area) {
    if (!area) return;
    area.style.height = "auto";
    area.style.height = `${area.scrollHeight + 2}px`;
  }

  function renderDescription() {
    const box = $("wf-description"), w = workflow();
    box.hidden = !w;
    if (!w) { box.replaceChildren(); return; }
    if (!descriptionEdit) {
      const beginEdit = () => {
        if (state.saving) return;
        descriptionEdit = { name: w.name, version: shownVersion(), original: w.description || "", value: singleLineDescription(w.description), mode: "text", saving: false, error: "" };
        renderDescription(); renderHead();
        box.querySelector("textarea")?.focus();
      };
      const text = h("p", { className: "wf-description-text", tabIndex: 0,
        title: "Double-click to edit this version's description" }, markdownInline(singleLineDescription(w.description) || "No description"));
      text.addEventListener("dblclick", (ev) => { if (!ev.target.closest?.("a")) beginEdit(); });
      text.addEventListener("keydown", (ev) => {
        if (ev.target === text && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); beginEdit(); }
      });
      box.replaceChildren(text);
      return;
    }
    const draft = descriptionEdit;
    const area = h("textarea", { id: "wf-description-input", "aria-label": "Description", value: draft.value, rows: 1,
      disabled: draft.saving, placeholder: "No description", title: "Enter saves this version's description; Escape cancels" });
    const preview = h("p", { className: "wf-description-preview", tabIndex: 0 }, markdownInline(draft.value || "No description"));
    const toggle = h("button", { type: "button", className: "text-format-toggle", disabled: draft.saving });
    const editor = h("div", { className: "wf-description-editor" }, area, preview, toggle);
    const renderMode = () => {
      const markdown = draft.mode === "markdown";
      area.hidden = markdown;
      preview.hidden = !markdown;
      toggle.textContent = markdown ? "TXT" : "MD";
      toggle.title = markdown ? "Switch to plain text" : "Switch to Markdown";
      toggle.setAttribute("aria-label", toggle.title);
      if (!markdown) requestAnimationFrame(() => growDescription(area));
    };
    toggle.addEventListener("click", () => {
      draft.mode = draft.mode === "markdown" ? "text" : "markdown";
      renderMode();
      if (draft.mode === "text") area.focus();
    });
    const error = h("p", { className: "error-text", role: "alert", hidden: !draft.error }, draft.error);
    area.addEventListener("input", () => {
      const value = singleLineDescription(area.value);
      if (area.value !== value) area.value = value;
      draft.value = value;
      preview.replaceChildren(markdownInline(value || "No description"));
      growDescription(area); renderHead();
    });
    area.addEventListener("keydown", (ev) => {
      if (ev.isComposing) return;
      if (ev.key === "Enter") { ev.preventDefault(); storeDescription(draft); }
      if (ev.key === "Escape") {
        ev.preventDefault(); descriptionEdit = null; renderDescription(); renderHead();
      }
    });
    area.addEventListener("beforeinput", (ev) => {
      if (ev.inputType === "insertParagraph" || ev.inputType === "insertLineBreak") ev.preventDefault();
    });
    editor.addEventListener("focusout", (ev) => {
      if (!editor.contains(ev.relatedTarget) && descriptionEdit === draft) storeDescription(draft);
    });
    editor.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape" && !ev.isComposing) {
        ev.preventDefault(); descriptionEdit = null; renderDescription(); renderHead();
      } else if (ev.target === preview && ev.key === "Enter") { ev.preventDefault(); storeDescription(draft); }
    });
    box.replaceChildren(editor, error);
    renderMode();
  }

  let descriptionWidth = 0;
  new ResizeObserver(() => {
    const box = $("wf-description");
    if (box.clientWidth === descriptionWidth) return;
    descriptionWidth = box.clientWidth;
    growDescription(box.querySelector("textarea"));
  }).observe($("wf-description"));

  async function storeDescription(draft) {
    if (draft !== descriptionEdit || draft.saving) return;
    if (!descriptionDirty()) { descriptionEdit = null; renderDescription(); renderHead(); return; }
    if (!draft.version) {
      workflow().description = draft.value;
      descriptionEdit = null;
      markDirty(); renderDescription();
      return;
    }
    draft.saving = true;
    draft.error = "";
    renderDescription(); renderHead();
    try {
      const result = await api("PATCH", `${API}/workflows/${encodeURIComponent(draft.name)}/versions/${draft.version}/description`, { description: draft.value }, creatorHeaders());
      const description = result.description || "";
      const listed = state.workflows.find((item) => item.name === draft.name && Number(item.version) === draft.version);
      if (listed) listed.description = description;
      versionCache.delete(WorkflowRef.format(draft.name, draft.version));
      if (workflow()?.name === draft.name && shownVersion() === draft.version) workflow().description = description;
      if (descriptionEdit === draft) descriptionEdit = null;
      document.dispatchEvent(new CustomEvent("workflowdescriptionchange", { detail: { name: draft.name, version: draft.version, description } }));
      setSaveState("Saved");
    } catch (err) {
      draft.error = `Not saved: ${namedError(err)}`;
    } finally {
      draft.saving = false;
      renderDescription(); renderHead();
    }
  }

  $("wf-new-slot").append(iconButton("plus", "New workflow", () => newWorkflow()));
  $("wf-list").parentElement.addEventListener("contextmenu", (ev) => {
    if (ev.target.closest?.(".tree-row")) return;
    openMenu(ev, newItems(""));
  });

  // Names are stored as lowercase-with-dashes IDs and shown in title case.
  // slug turns a typed name into an ID: words joined by dashes, lowercase except a word typed in
  // capitals ("API Review" becomes API-review and shows as API Review).
  const slug = (text) => text.split(/[^A-Za-z0-9]+/).filter(Boolean)
    .map((w) => (!/[a-z]/.test(w) && /[A-Z].*[A-Z]/.test(w) ? w : w.toLowerCase())).join("-").slice(0, 63).replace(/-+$/, "");
  const sameName = (a, b) => a.toLowerCase() === b.toLowerCase(); // names differing only in case are one name
  // groupSlug turns a typed group into its ID, part by part: "Ecc / Code Review" becomes ecc/code-review.
  const groupSlug = (text) => text.split("/").map(slug).filter(Boolean).join("/");

  // nameFields asks for a group and a name, as on Agents and Policies: the group is required
  // (nested groups separated with /, suggested from the existing ones), the name has no slash.
  // Both show in title case and are saved as IDs. check returns the full ID, or "" after showing
  // under the field what is wrong; except is the current name of the workflow being renamed.
  function nameFields(idPrefix, value) {
    const group = h("input", { autocomplete: "off", spellcheck: false, id: `${idPrefix}-group`, value: titleCase(groupOf(value)), list: `${idPrefix}-groups` });
    const name = h("input", { autocomplete: "off", spellcheck: false, id: `${idPrefix}-name`, value: titleCase(leafOf(value)) });
    const initial = [group.value, name.value];
    const groupProblem = h("p", { className: "error-text", role: "alert" });
    const nameProblem = h("p", { className: "error-text", role: "alert" });
    const row = h("div", { className: "name-fields" },
      h("div", { className: "field" }, h("label", { htmlFor: group.id, title: "Nested groups are separated with /" }, "Group"), group, groupProblem,
        h("datalist", { id: `${idPrefix}-groups` }, ...[...groupList()].sort().map((g) => h("option", { value: titleCase(g) })))),
      h("div", { className: "field" }, h("label", { htmlFor: name.id }, "Name"), name, nameProblem));
    const check = (except = "") => {
      groupProblem.textContent = nameProblem.textContent = "";
      const fail = (p, field, text) => { p.textContent = text; field.focus(); return ""; };
      const g = groupSlug(group.value), n = slug(name.value), full = `${g}/${n}`;
      if (!g) return fail(groupProblem, group, group.value.trim() ? "Use letters, digits, and dashes; separate nested groups with /." : "Enter a group.");
      if (g.split("/").length > 3) return fail(groupProblem, group, "Groups nest at most three deep.");
      if (!n) return fail(nameProblem, name, name.value.trim() ? "Use letters, digits, and dashes." : "Enter a name.");
      // A draft never stands in the way: it is the one renamed, or the one a new workflow replaces.
      const taken = state.workflows.find((w) => w.name !== except && !isDraft(w) && sameName(w.name, full));
      if (taken) return fail(nameProblem, name, `There is already a workflow named ${titleCase(taken.name)}.`);
      const folder = [...groupList()].find((x) => sameName(x, full));
      if (folder) return fail(nameProblem, name, `${titleCase(folder)} is a group.`);
      return full;
    };
    const unchanged = () => group.value === initial[0] && name.value === initial[1];
    return { row, group, name, check, unchanged };
  }

  // nameDialog opens a dialog with the Group and Name fields; submit gets the fields and closes
  // it by resolving true. A submit on its way ignores further clicks and Enter.
  function nameDialog(title, action, value, submit) {
    const id = nameFields("wf-name", value);
    const button = h("button", { type: "button", className: "primary" }, action);
    const dlg = openDialog({ title, body: id.row, footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), button] });
    const run = async () => {
      if (button.disabled) return;
      button.disabled = true;
      try { if (await submit(id)) dlg.close(); } finally { button.disabled = false; }
    };
    button.onclick = run;
    for (const c of [id.group, id.name]) c.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); run(); } });
    return id;
  }

  // renameWorkflow renames a workflow with every version, or moves it into another group.
  function renameWorkflow(i = state.current) {
    const e = state.workflows[i];
    if (!e) return;
    if (descriptionActionBlocked("renamed")) return;
    const id = nameDialog("Rename workflow", "Rename", e.name, async (fields) => {
      if (fields.unchanged()) return true;
      const to = fields.check(e.name);
      return !!to && (to === e.name || await renameTo(e, to));
    });
    // Renaming starts in the name with it selected (after the dialog focuses its first field).
    requestAnimationFrame(() => { id.name.focus(); id.name.select(); });
  }

  // newGroup asks for a name and adds the group; it is stored once it holds a workflow.
  function newGroup(parent) {
    const input = h("input", { autocomplete: "off", spellcheck: false, id: "wf-group-name" });
    const problem = h("p", { className: "error-text", role: "alert" });
    const create = h("button", { type: "button", className: "primary" }, "Create group");
    const dlg = openDialog({
      title: "New group",
      body: h("div", { className: "field" }, h("label", { htmlFor: input.id }, "Name"), input, problem),
      footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), create],
    });
    const submit = () => {
      const n = slug(input.value), path = [parent, n].filter(Boolean).join("/");
      const fail = (text) => { problem.textContent = text; input.focus(); };
      if (!n) return fail(input.value.trim() ? "Use letters, digits, and dashes." : "Enter a name.");
      if (path.split("/").length > 3) return fail("Groups nest at most three deep.");
      if (state.workflows.some((w) => sameName(w.name, path))) return fail(`${titleCase(path)} is a workflow.`);
      const known = [...groupList()].find((g) => sameName(g, path));
      groups.add(known || path);
      dlg.close();
      renderList();
      tree.reveal([...groupIds(known || path, "g:"), `g:${known || path}`]).then(() => tree.select(`g:${known || path}`));
    };
    create.onclick = submit;
    input.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); submit(); } });
  }

  // renameTo renames (or moves) a workflow: the orchestrator renames every version and follows
  // the name in the other workflows; the page does the same with what it shows. A draft has only
  // its name here. It resolves false when the orchestrator refused.
  async function renameTo(e, to) {
    const from = e.name;
    if (!isDraft(e)) {
      try { await api("POST", `${API}/workflows/${encodeURIComponent(from)}/rename`, { name: to }, creatorHeaders()); }
      catch (err) { setStatus(`Not renamed: ${namedError(err)}`, "error"); return false; }
    }
    const shown = e === entry();
    e.name = to;
    // Workflow nodes and teams keep pointing at it: in the list, on the canvas, and in the
    // stored version the canvas is compared with.
    const base = state.baseline ? JSON.parse(state.baseline) : null;
    L.renameWorkflowRefs([...state.workflows, state.edit, base].filter(Boolean), from, to);
    if (base) state.baseline = JSON.stringify(base);
    if (shown && state.edit) state.edit.name = to;
    state.path = state.path.map((ref) => (WorkflowRef.nameOf(ref) === from ? WorkflowRef.format(to, WorkflowRef.parse(ref).version) : ref));
    versionCache.clear();
    if (shown && view.fitted) saveView(to);
    try { localStorage.removeItem(viewKey(from)); } catch { /* storage unavailable */ }
    if (!isDraft(e)) setSaveState("Renamed");
    render();
    return true;
  }

  // renderCanvasMessage explains an empty canvas: no workflows, no steps, or a load error.
  function renderCanvasMessage() {
    const box = $("canvas-message");
    const w = workflow();
    let content = null;
    if (state.loadError) content = errorPanel(state.loadError, load);
    else if (!state.loaded) content = null;
    else if (!w) content = emptyPanel("No workflows yet", { label: "New workflow", primary: true, run: () => newWorkflow() });
    else if (!w.steps.length) content = emptyPanel("No steps yet", { label: "Add step", run: () => addStep() });
    box.hidden = !content;
    box.replaceChildren(content || "");
  }

  // The view is the pan offset and zoom of the canvas. It is centred once when a workflow
  // is opened or auto-arranged and then only changes by panning (a drag on the empty canvas, or Shift+drag) and zooming
  // (wheel), never by an edit, so tiles stay exactly where they are put.
  const view = { ox: 0, oy: 0, scale: 1, fitted: false, width: 0 };
  const MIN_SCALE = 0.25, MAX_SCALE = 3;

  function resetView() { view.fitted = false; view.touched = false; }

  // Views are remembered per workflow, in this session and across reloads.
  const viewKey = (name) => `factory.view.${name}`;
  function saveView(name) {
    try { localStorage.setItem(viewKey(name), JSON.stringify({ ox: view.ox, oy: view.oy, scale: view.scale })); } catch { /* storage unavailable */ }
  }
  function restoreView(name) {
    try {
      const v = JSON.parse(localStorage.getItem(viewKey(name)) || "null");
      if (!v || ![v.ox, v.oy, v.scale].every(Number.isFinite)) return false;
      Object.assign(view, v);
      return true;
    } catch { return false; }
  }

  // placeGraph restores or fits the view the first time a workflow is drawn and returns the graph group.
  function placeGraph(svg, w) {
    const visible = svg.parentElement.clientWidth;
    view.width = visible;
    if (view.fitted && !view.touched && visible !== view.fitWidth) view.fitted = false;
    if (!view.fitted && visible && restoreView(w.name)) { view.fitted = true; view.touched = true; }
    if (!view.fitted && visible) {
      const ip = inputPos(w);
      const boxes = [...w.steps.map((s) => ({ ...s.position, w: TILE_W, h: TILE_H })), ...(ip ? [{ ...ip, w: TILE_W, h: TILE_H }] : [])];
      // Arrows that skip rows run in lanes beside the tiles; the fit keeps those in view too.
      for (const s of w.steps) for (const d of s.depends_on || []) {
        const f = step(d);
        const x = f ? laneX(outPort(f), inPort(s), bowsLeft(w, f, s)) : null;
        if (x !== null) boxes.push({ x: x - 8, y: s.position.y, w: 16, h: TILE_H });
      }
      // So do the back edges, which run around the tiles.
      for (const s of w.steps) for (const p of backEdge(w, s)?.points || []) boxes.push({ x: p.x - 8, y: p.y - 8, w: 16, h: 16 });
      const minX = Math.min(...boxes.map((b) => b.x)), maxX = Math.max(...boxes.map((b) => b.x + b.w));
      const minY = Math.min(...boxes.map((b) => b.y));
      const graphW = maxX - minX;
      // A graph wider than the canvas starts zoomed out to fit (down to half size).
      view.scale = Math.max(0.5, Math.min(1, (visible - 2 * GRAPH_MARGIN) / graphW));
      view.ox = Math.max(GRAPH_MARGIN, (visible - graphW * view.scale) / 2) - minX * view.scale;
      view.oy = GRAPH_MARGIN - minY * view.scale;
      view.fitted = true;
      view.fitWidth = visible;
    }
    return el("g", { class: "graph", transform: `translate(${view.ox},${view.oy}) scale(${view.scale})` }, svg);
  }

  new ResizeObserver(() => {
    const width = $("canvas-scroll").clientWidth;
    if (!width || width === view.width || !workflow()) return;
    renderCanvas();
  }).observe($("canvas-scroll"));

  // Wheel zooms around the pointer; the point under the cursor stays put.
  $("canvas-scroll").addEventListener("wheel", (ev) => {
    if (!workflow()) return;
    ev.preventDefault();
    const r = $("canvas").getBoundingClientRect();
    const cx = ev.clientX - r.left, cy = ev.clientY - r.top;
    const next = Math.min(MAX_SCALE, Math.max(MIN_SCALE, view.scale * Math.exp(-ev.deltaY * 0.0015)));
    const k = next / view.scale;
    view.ox = cx - (cx - view.ox) * k;
    view.oy = cy - (cy - view.oy) * k;
    view.scale = next;
    view.touched = true;
    saveView(workflow().name);
    renderCanvas();
  }, { passive: false });

  // The team opened by a double-click on its node is shown here, over the canvas.
  const teamCanvas = h("div", { id: "team-canvas", className: "team-canvas", hidden: true });
  $("canvas-scroll").append(teamCanvas);

  function renderCanvas() {
    const svg = $("canvas");
    svg.replaceChildren();
    const defs = el("defs", {}, svg);
    const marker = el("marker", { id: "arrow", viewBox: "0 0 10 10", refX: 2, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse" }, defs);
    el("path", { d: "M0,0 L10,5 L0,10 z", fill: "var(--edge)" }, marker);
    const rejectMarker = el("marker", { id: "arrow-reject", viewBox: "0 0 10 10", refX: 2, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse" }, defs);
    el("path", { d: "M0,0 L10,5 L0,10 z", fill: "var(--failed)" }, rejectMarker);
    renderCanvasMessage();
    const w = workflow();
    // An opened team takes the place of the graph: its members and channels, as in the node panel.
    const team = state.teamOpen ? step(state.teamOpen.step) : null;
    teamCanvas.hidden = !team;
    svg.style.display = team ? "none" : "";
    teamCanvas.replaceChildren(team ? teamPanel(team) : "");
    if (team) return;
    if (!w || !w.steps.length) return;
    const root = placeGraph(svg, w);

    for (const s of w.steps) {
      for (const d of s.depends_on || []) {
        const from = step(d);
        if (!from) continue;
        const g = el("g", {}, root);
        el("title", {}, g).textContent = `${titleCase(s.id)} depends on ${titleCase(d)}; a click removes it`;
        const path = dependencyPath(w, from, s);
        el("path", { d: path, class: "edge-hit" }, g);
        el("path", { d: path, class: "edge", "marker-end": "url(#arrow)" }, g);
        g.onclick = () => removeDependency(s, d);
        g.addEventListener("contextmenu", (ev) => openMenu(ev, [["Remove dependency", () => removeDependency(s, d), "danger", `${titleCase(s.id)} no longer receives the output of ${titleCase(d)}`]]));
      }
    }

    // A gate's back edge: where its rejections go. Dashed, in the failed colour, around the tiles.
    for (const s of w.steps) {
      const route = backEdge(w, s);
      if (!route) continue;
      const g = el("g", { class: "back-edge-group", "data-gate": s.id }, root);
      el("title", {}, g).textContent = L.backEdgeTip(s, step(s.on_reject), titleCase);
      el("path", { d: route.d, class: "edge-hit" }, g);
      el("path", { d: route.d, class: "back-edge", "marker-end": "url(#arrow-reject)" }, g);
      g.onclick = () => removeBackEdge(s);
      g.addEventListener("contextmenu", (ev) => openMenu(ev, [["Remove back edge", () => removeBackEdge(s), "danger", `A rejection at ${titleCase(s.id)} ends the run again`]]));
    }

    const ip = inputPos(w);
    if (ip) {
      const savedIds = new Set((state.baseline ? JSON.parse(state.baseline).steps : []).map((s) => s.id));
      for (const s of w.steps.filter((s) => savedIds.has(s.id) && !(s.depends_on || []).length)) {
        el("path", { d: edgePath({ x: ip.x + TILE_W / 2, y: ip.y + TILE_H }, inPort(s)), class: "edge input-edge", "marker-end": "url(#arrow)" }, root);
      }
      drawInputTile(root, defs, ip);
    }

    w.steps.forEach((s, i) => drawTile(root, defs, s, i));
  }

  // backEdge is the route of a gate's back edge to the step its rejections go to, or null for
  // a step without one. A team's outline reaches 8 px past its box, where its stack shows.
  function backEdge(w, s) {
    const target = s.service === "gate" && s.on_reject ? w.steps.find((o) => o.id === s.on_reject) : null;
    if (!target?.position || !s.position || target === s) return null;
    return L.backEdgeRoute(s.position, target.position, w.steps.filter((o) => o !== s && o !== target && o.position).map((o) => o.position),
      { w: TILE_W, h: TILE_H }, target.service === "team" ? 8 : 0);
  }

  // inputPos places the input node one row above the first steps, centred over them.
  function inputPos(w) {
    const roots = w.steps.filter((s) => !(s.depends_on || []).length);
    if (!roots.length) return null;
    const minX = Math.min(...roots.map((s) => s.position.x)), maxX = Math.max(...roots.map((s) => s.position.x + TILE_W));
    return { x: Math.round((minX + maxX) / 2 - TILE_W / 2), y: Math.min(...roots.map((s) => s.position.y)) - ROW_H };
  }

  function drawInputTile(root, defs, p) {
    const g = el("g", { class: "tile input-tile", transform: `translate(${p.x},${p.y})`, tabindex: 0, role: "button", "aria-label": "Input" }, root);
    el("title", {}, g).textContent = "Opens Select Inputs to start runs";
    el("rect", { class: "body", width: TILE_W, height: TILE_H, rx: 8 }, g);
    const ic = el("g", { class: "type-icon", transform: "translate(12,24)", stroke: "currentColor", fill: "none", "stroke-width": 1.5, "stroke-linecap": "round", "stroke-linejoin": "round" }, g);
    ic.innerHTML = ICONS.run;
    el("text", { x: 34, y: 37, class: "name" }, g).textContent = "Input";
    el("circle", { class: "port", cx: TILE_W / 2, cy: TILE_H, r: 5 }, g);
    const open = (ev) => { ev.stopPropagation(); openInputs(); };
    g.addEventListener("pointerdown", (ev) => ev.stopPropagation());
    g.addEventListener("click", open);
    g.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); open(ev); } });
  }

  // The outline of a tile comes from WorkflowLogic.tileOutline. Icon and text move in by this
  // much where a band or a slanted edge takes the room.
  const TILE_PAD = { workflow: BAND_W, gate: 6, interactive: 8, merge: 12, devpod: 10, command: 8, "devpod-stop": 12 };
  const TRAY_LINE_Y = 8; // a batch's second line, under its top edge

  function drawTile(root, defs, s, i) {
    const W = TILE_W, H = TILE_H;
    const g = el("g", {
      class: `tile kind-${kind(s)}${state.selected === s.id ? " selected" : ""}${missingAgent(s) ? " missing" : ""}`,
      transform: `translate(${s.position.x},${s.position.y})`,
      tabindex: 0, role: "button", "data-step": s.id,
      "aria-label": `Step ${s.id}:${subtitle(s) ? " " + subtitle(s) : ""}`,
    }, root);
    // Every kind of step has its own outline, all inside the same box so ports and arrows stay put.
    const outline = L.tileOutline(kind(s), W, H);
    const clip = el("clipPath", { id: `tile-clip-${i}` }, defs);
    el("path", { d: outline }, clip);
    // A team is a stack of tiles: two more outlines show behind the front one.
    if (s.service === "team") for (const d of [8, 4]) el("rect", { class: "stack", x: d, y: -d, width: W, height: H, rx: 8 }, g);
    const body = el("path", { class: "body", d: outline }, g);
    // A workflow node has a band down each side, the flowchart sign for a process defined elsewhere:
    // it holds a whole workflow. Its icon and text move in by the band, as they do by a slanted edge.
    const pad = TILE_PAD[s.service] || 0;
    if (s.service === "workflow") {
      for (const x of [1, W - pad]) el("rect", { class: "band", x, y: 1, width: pad - 1, height: H - 2, "clip-path": `url(#tile-clip-${i})` }, g);
      for (const x of [pad, W - pad]) el("line", { class: "band-line", x1: x, y1: 1, x2: x, y2: H - 1 }, g);
    }
    // A batch is a tray: a second line runs under its top edge.
    if (s.service === "batch") el("line", { class: "tray-line", x1: 1, y1: TRAY_LINE_Y, x2: W - 1, y2: TRAY_LINE_Y }, g);
    const ic = el("g", { class: "type-icon", transform: `translate(${12 + pad},13)`, stroke: "currentColor", fill: "none", "stroke-width": 1.5, "stroke-linecap": "round", "stroke-linejoin": "round" }, g);
    ic.innerHTML = KIND_ICON[s.service] || ICONS[TYPE_ICON[s.service] || "step"];
    const sub = subtitle(s);
    if (!sub) ic.setAttribute("transform", `translate(${12 + pad},24)`); // a one-line tile centres its name
    const shownName = fit(titleCase(s.id), W - 46 - 2 * pad, 8), shownSub = sub ? fit(sub, W - 24 - 2 * pad) : "";
    // A tooltip only when the tile cuts its name or second line off.
    if (missingAgent(s)) el("title", {}, g).textContent = `${titleCase(s.id)}: ${missingRef(s)} no longer exists`; // the reason, always
    else if (shownName !== titleCase(s.id) || shownSub !== sub) el("title", {}, g).textContent = sub ? `${titleCase(s.id)}: ${sub}` : titleCase(s.id);
    el("text", { x: 34 + pad, y: sub ? 26 : 37, class: "name" }, g).textContent = shownName;
    if (sub) el("text", { x: 12 + pad, y: 50, class: "sub" }, g).textContent = shownSub;
    el("title", {}, el("circle", { class: "port", cx: W / 2, cy: 0, r: 5 }, g)).textContent = s.service === "route"
      ? "Receives the one output whose route field names the workflow to start"
      : (s.depends_on || []).length ? "Receives the outputs of the steps it depends on" : "Receives the run input";
    body.onpointerdown = (ev) => startMove(ev, s);
    for (const t of g.querySelectorAll("text, .type-icon")) t.onpointerdown = (ev) => startMove(ev, s);
    // Nothing may depend on a route step, so it has no output port.
    if (s.service !== "route") {
      const out = el("circle", { class: "port", cx: W / 2, cy: H, r: 6 }, g);
      el("title", {}, out).textContent = "Creates a dependency when dragged onto another step";
      out.onpointerdown = (ev) => startConnect(ev, s);
    }
    // A gate has a second output port, Rejected, on its right edge below the corner: clear of
    // the output port at the bottom and of the arrows that enter a tile's side at its middle.
    if (s.service === "gate") {
      const rejected = el("circle", { class: "port port-reject", cx: W, cy: L.REJECT_PORT_Y, r: 5 }, g);
      el("title", {}, rejected).textContent = "Rejected: sends a rejection back to an earlier step when dragged onto it";
      rejected.onpointerdown = (ev) => startReject(ev, s);
    }
    g.addEventListener("contextmenu", (ev) => {
      if (state.selected !== s.id) { state.selected = s.id; render(); }
      openMenu(ev, stepActions(s));
    });
    g.addEventListener("keydown", (ev) => {
      if (ev.target !== g) return;
      if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); state.selected = s.id; render(); focusTile(s.id); }
    });
  }

  const focusTile = (id) => [...$("canvas").querySelectorAll(".tile")].find((t) => t.getAttribute("aria-label")?.startsWith(`Step ${id}:`))?.focus();

  // ---- Inspector ----

  function renderInspector() {
    const box = $("inspector");
    const w = workflow();
    const s = state.selected && step(state.selected);
    $("tab-workflows").classList.toggle("no-inspector", !state.inspectorOpen);
    if (!w) { box.replaceChildren(); return; }
    if (s) {
      const card = usesAgent(s) && s.agent ? agentCard(s.agent)
        : s.service === "team" ? teamPanel(s) : s.service === "workflow" ? nestedCard(s) : null;
      // What the orchestrator would refuse on Save shows under the fields, like a team's problems.
      const problems = L.stepProblems(w, s, titleCase).map((p) => h("p", { className: "error-text team-problem" }, p.replace(/: (.+)$/, (_, id) => `: ${titleCase(id)}`)));
      box.replaceChildren(
        h("div", { className: "panel-head" }, h("h2", {}, typeName(s))),
        h("div", { className: "panel-body" },
          h("div", { className: "section" }, stepForm(s), ...problems),
          card ? h("div", { className: "section" }, card) : null));
      return;
    }
    box.replaceChildren(h("div", { className: "panel-head" }), h("div", { className: "panel-body" }));
  }

  // stepFields applies one setting edited in the inspector.
  const stepFields = {
    id(s, typed) {
      const v = slug(typed);
      if (!v) { setStatus("Not renamed: a node needs an ID", "error"); return; }
      if (workflow().steps.some((o) => o !== s && sameName(o.id, v))) { setStatus(`There is already a node named ${titleCase(v)}`, "error"); return; }
      for (const o of workflow().steps) o.depends_on = (o.depends_on || []).map((d) => (d === s.id ? v : d));
      for (const o of workflow().steps) if (o.on_reject === s.id) o.on_reject = v;
      if (state.selected === s.id) state.selected = v;
      s.id = v;
    },
    service(s, v) {
      s.service = v;
      if (v !== "agent" && v !== "interactive" && v !== "team") delete s.agent;
      if (v !== "interactive" && v !== "team") delete s.agents;
      // An interactive step needs its main agent, a team its lead; it starts with the first one, like a new step.
      if ((v === "interactive" || v === "team") && !s.agent && state.agentRefs[0]) s.agent = state.agentRefs[0];
      if (v !== "gate") { delete s.question; delete s.on_reject; }
      if (SPECIAL.includes(v)) delete s.activity;
      if (v !== "team") { delete s.workflows; delete s.channels; delete s.max_turns; delete s.member_positions; }
      if (v === "team") {
        // Further agents taken over from another kind stay only while their names differ.
        const names = new Set([L.memberName(s.agent)]);
        const kept = (s.agents || []).filter((ref) => ref !== s.agent && !names.has(L.memberName(ref)) && names.add(L.memberName(ref)));
        if (kept.length) s.agents = kept; else delete s.agents;
      }
      // A workflow step starts with the first workflow it may run.
      if (v !== "workflow") delete s.workflow;
      else if (!s.workflow) { const first = nestable()[0]; if (first) s.workflow = pinNewest(first); }
      if (v === "workflow" || v === "team") delete s.max_attempts;
      if (v === "workflow") delete s.timeout;
      // A merge or batch keeps only its parameters, a dev pod step only its own fields; any
      // other kind loses them.
      L.dropForeignFields(s);
      if (v === "devpod") podDefaults(s);
    },
    // A dev pod step: a chosen repository brings its default branch as the source branch.
    repository(s, v) {
      if (v) s.repository = v; else delete s.repository;
      const repo = state.repos.find((r) => r.name === v);
      if (repo && !s.source_branch) s.source_branch = repo.defaultBranch;
    },
    source_branch(s, v) { if (v) s.source_branch = v; else delete s.source_branch; },
    work_branch(s, v) { if (v) s.work_branch = v; else delete s.work_branch; },
    name(s, v) { if (v) s.name = v; else delete s.name; },
    protected(s, v) { const paths = L.linesOf(v); if (paths.length) s.protected = paths; else delete s.protected; },
    // A command step: one command, on one line.
    command(s, v) { v = v.replace(/\s*\n\s*/g, " "); if (v) s.command = v; else delete s.command; },
    allow_failure(s, on) { if (on === true) s.allow_failure = true; else delete s.allow_failure; },
    max_rejections(s, v) { const n = L.paramValue(v); if (n === undefined) delete s.max_rejections; else s.max_rejections = n; },
    // The parameters of a merge or batch: whole numbers, left out while empty.
    capacity(s, v) { const n = L.paramValue(v); if (n === undefined) delete s.capacity; else s.capacity = n; },
    size(s, v) { const n = L.paramValue(v); if (n === undefined) delete s.size; else s.size = n; },
    input_timeout(s, v) { const n = L.paramValue(v); if (n === undefined) delete s.input_timeout; else s.input_timeout = n; },
    // A chosen workflow is pinned to its newest version; Version pins another one.
    workflow(s, v) { if (v) s.workflow = pinNewest(v); else delete s.workflow; },
    workflow_version(s, v) { const n = Number(v); if (s.workflow && n > 0) s.workflow = WorkflowRef.format(WorkflowRef.nameOf(s.workflow), n); },
    on_reject(s, v) { if (v) s.on_reject = v; else { delete s.on_reject; delete s.max_rejections; } },
    max_turns(s, v) { const n = parseInt(v, 10); if (n > 0) s.max_turns = n; else delete s.max_turns; },
    // lead: another agent leads the team; a member that becomes the lead swaps places with the old one.
    lead(s, v) {
      if (!v) return;
      const next = { ...s, ...L.setLead(s, v) };
      const problem = L.teamProblems({ ...next, channels: undefined })[0];
      if (problem) { setStatus(problem, "error"); return; }
      applyTeam(s, next);
    },
    agent(s, v) {
      if (v) s.agent = v; else delete s.agent;
      // The main agent is not listed again among the further agents.
      if (s.agents) { s.agents = s.agents.filter((ref) => ref !== v); if (!s.agents.length) delete s.agents; }
    },
    agents(s, refs) {
      const list = [...new Set(refs)].filter((ref) => ref && ref !== s.agent);
      if (list.length) s.agents = list; else delete s.agents;
    },
    question(s, v) { v = v.replace(/\s*\n\s*/g, " "); if (v) s.question = v; else delete s.question; }, // one sentence
    activity(s, v) { if (v) s.activity = v; else delete s.activity; },
    timeout(s, v) { if (v) s.timeout = v; else delete s.timeout; },
    attempts(s, v) { const n = parseInt(v, 10); if (n >= 0) s.max_attempts = n + 1; else delete s.max_attempts; },
  };

  // podDefaults gives a new dev pod step what it starts with: the default work branch and, as
  // a new agent step gets the first agent, the first repository with its default branch.
  function podDefaults(s) {
    if (!s.work_branch) s.work_branch = L.DEFAULT_WORK_BRANCH;
    const first = state.repos[0];
    if (!s.repository && first) { s.repository = first.name; if (!s.source_branch) s.source_branch = first.defaultBranch; }
  }

  // editRules changes the rules of a dev pod step; a step without rules stores none.
  function editRules(s, change) {
    const current = step(s.id);
    if (!current) return;
    const rules = change(Array.isArray(current.rules) ? [...current.rules] : []);
    if (rules.length) current.rules = rules; else delete current.rules;
    markDirty();
    refreshAfterEdit();
  }

  // rulesEditor is the small list editor of a dev pod's rules: each blocks a tool call of the
  // agent (by tool, path and content or command) and answers it with a message.
  function rulesEditor(s) {
    const rules = Array.isArray(s.rules) ? s.rules : [];
    const part = (i, key, label, control, tip, className = "") => {
      control.id = `inspector-rule-${i}-${key}`;
      control.dataset.field = `rule-${i}-${key}`; // focus returns here after a redraw; stepFields has no such field
      control.addEventListener("change", () => editRules(s, (list) => { if (list[i]) list[i] = L.ruleWith(list[i], key, control.value); return list; }));
      return h("div", { className: `field ${className}`.trim() }, h("label", { htmlFor: control.id, title: tip }, label), control);
    };
    const text = (value, placeholder) => h("input", { value: value ?? "", placeholder, autocomplete: "off", spellcheck: false });
    const add = iconButton("plus", "Add rule", () => editRules(s, (list) => [...list, { tool: L.RULE_TOOLS[0], message: "" }]));
    return h("div", { className: "field field-wide" },
      h("div", { className: "pod-rules-head" }, h("label", { id: "inspector-rules-label", title: "A rule blocks a tool call of the agent in the dev pod and answers it with its message" }, "Rules"), add),
      h("div", { className: "pod-rules", role: "group", "aria-labelledby": "inspector-rules-label" },
        rules.length ? rules.map((rule, i) => h("div", { className: "pod-rule", role: "group", "aria-label": `Rule ${i + 1}` },
          part(i, "tool", "Tool", h("select", {}, ...L.RULE_TOOLS.map((tool) => h("option", { value: tool, selected: tool === rule.tool }, tool))), "The tool call the rule blocks"),
          part(i, "path", "Path", text(rule.path, "Any path"), "A glob such as src/panel/**; the rule applies to files that match it"),
          part(i, "matches", "Matches", text(rule.matches, "Any content"), "A regular expression on the new content of the file, or on the command"),
          iconButton("close", `Remove rule ${i + 1}`, () => editRules(s, (list) => list.filter((_, at) => at !== i))),
          part(i, "message", "Message", text(rule.message, "Not set"), "What the agent is told: the rule, the reason and the fix", "pod-rule-message")))
          : h("p", { className: "none" }, "No rules")));
  }

  // stepForm builds the inspector fields of a step.
  function stepForm(s) {
    const fid = (name) => `inspector-${name}`;
    const field = (label, name, control, short = false, tip = "") => {
      control.id = fid(name);
      control.dataset.field = name;
      return h("div", { className: `field${short ? " short" : ""}` }, h("label", { htmlFor: control.id, title: tip }, label), control);
    };
    const input = (value, props = {}) => h("input", { value: value ?? "", autocomplete: "off", ...props });
    const id = input(titleCase(s.id)); // shown like the tile name; saved as an ID
    const activity = SPECIAL.includes(s.service) ? null : input(s.activity, { placeholder: "Process" });
    const service = input(s.service);
    service.setAttribute("list", "services");
    const agent = h("select");
    const refs = [...state.agentRefs];
    if (s.agent && !refs.includes(s.agent)) refs.unshift(s.agent);
    // A gate's question is a sentence: its box wraps and grows with it, so all of it shows.
    const question = s.service === "gate" ? h("textarea", { rows: 1, value: s.question ?? "" }) : null;
    agent.replaceChildren(h("option", { value: "" }, "No agent"), ...refs.map((r) => h("option", { value: r, selected: r === s.agent }, state.agentsLoaded && !state.agentRefs.includes(r) ? `${r} (missing)` : r)));
    // An interactive step's further agents: a compact multi-select like Policies on the Agents page.
    const more = s.service === "interactive" ? agentsPicker(s) : null;
    // A workflow step runs one version of one of the workflows that do not lead back to this one.
    // The step stores both as one reference, name@vN.
    let nested = null, nestedVersion = null;
    if (s.service === "workflow") {
      const pin = WorkflowRef.parse(s.workflow || "");
      const names = nestable();
      if (pin.name && !names.includes(pin.name)) names.unshift(pin.name);
      nested = h("select", {}, pin.name ? null : h("option", { value: "" }, "Not set"),
        ...names.map((n) => h("option", { value: n, selected: n === pin.name }, missingWorkflow(n) ? `${titleCase(n)} (missing)` : titleCase(n))));
      if (pin.name) {
        const options = L.versionOptions(stored().find((w) => w.name === pin.name)?.versions, pin.version);
        const chosen = pin.version || newestOf(pin.name); // a reference without a version runs the newest
        nestedVersion = h("select", {}, ...options.map((o) => h("option", { value: String(o.version), selected: o.version === chosen }, o.label)));
      }
    }
    // A team's lead is one of the agents; its other members are chosen like an interactive step's agents.
    let lead = null;
    if (s.service === "team") {
      lead = h("select", {}, s.agent ? null : h("option", { value: "" }, "No agent"),
        ...refs.map((r) => h("option", { value: r, selected: r === s.agent }, state.agentsLoaded && !state.agentRefs.includes(r) ? `${r} (missing)` : r)));
    }
    // A gate can send a rejection back to an earlier step it depends on: the allowed ones are
    // offered, and one that is set but not allowed is listed as such (its problem shows below).
    let onReject = null;
    if (s.service === "gate") {
      const targets = L.rejectTargets(workflow(), s);
      const listed = s.on_reject && !targets.includes(s.on_reject) ? [s.on_reject, ...targets] : targets;
      onReject = h("select", {}, h("option", { value: "" }, "Nobody: a rejection ends the run"),
        ...listed.map((t) => h("option", { value: t, selected: t === s.on_reject }, targets.includes(t) ? titleCase(t) : `${titleCase(t)} (not allowed)`)));
    }
    // A dev pod step clones one of the repositories of the Connectors page.
    let repository = null;
    if (s.service === "devpod") {
      const names = state.repos.map((r) => r.name);
      if (s.repository && !names.includes(s.repository)) names.unshift(s.repository);
      repository = h("select", { title: state.reposError || undefined }, s.repository ? null : h("option", { value: "" }, "Not set"),
        ...names.map((n) => h("option", { value: n, selected: n === s.repository }, state.reposLoaded && !state.repos.some((r) => r.name === n) ? `${n} (missing)` : n)));
    }
    // A command is one line of shell: its box wraps and grows with it, like a gate's question.
    const command = s.service === "command" ? h("textarea", { rows: 1, className: "code", value: s.command ?? "", placeholder: "Not set", spellcheck: false }) : null;
    const allowFailure = s.service === "command" ? h("input", { type: "checkbox", checked: s.allow_failure === true }) : null;
    const protectedPaths = s.service === "devpod" ? h("textarea", { rows: 2, className: "pod-lines", value: (s.protected || []).join("\n"), placeholder: "No protected paths", spellcheck: false }) : null;
    // A merge or batch shows its sources (changed by drawing arrows, as for every step) and its
    // parameters: plain digit fields whose placeholder is the default.
    const node = L.isNode(s);
    let sources = null;
    if (node) {
      const names = (s.depends_on || []).map(titleCase);
      const list = h("div", { className: `node-sources${names.length ? "" : " none"}`, role: "group", "aria-labelledby": fid("sources-label") }, names.length ? names.join(", ") : "No sources");
      sources = h("div", { className: "field" },
        h("label", { id: fid("sources-label"), title: s.service === "merge"
          ? "The steps whose results the merge waits for, forwarded in this order; an arrow drawn onto the node adds one, a click on an arrow removes it"
          : "The steps whose results the batch collects; an arrow drawn onto the node adds one, a click on an arrow removes it" }, "Sources"), list);
    }
    const digits = (name, placeholder) => input(s[name] ?? "", { placeholder, inputMode: "numeric", size: 4, maxLength: 6 });
    // Input timeout is in seconds; the unit shows beside the field.
    const seconds = (control) => h("div", { className: "row unit-row" }, control, h("span", { className: "unit", "aria-hidden": "true" }, "s"));
    const timeoutField = (tip) => {
      const control = digits("input_timeout", String(L.NODE_PARAMS[s.service].input_timeout));
      control.id = fid("input_timeout");
      control.dataset.field = "input_timeout";
      return h("div", { className: "field short" }, h("label", { htmlFor: control.id, title: tip }, "Input timeout"), seconds(control));
    };
    const form = h("div", { className: "step-form" },
      h("div", { className: "field-row" },
        field("Node ID", "id", id),
        sources,
        nested ? field("Workflow", "workflow", nested, false, "The workflow this step runs; its final output is the output of this step") : null,
        nestedVersion ? field("Version", "workflow_version", nestedVersion, false, "The version of that workflow this step runs; it stays the same when the workflow gets newer versions") : null,
        lead ? field("Lead agent", "lead", lead, false, "The agent that receives the task, instructs the members, and finishes the team") : null,
        lead ? field("Agents", "team-agents", membersPicker(s, "agents"), false, "Further agents in the team") : null,
        lead ? field("Workflows", "team-workflows", membersPicker(s, "workflows"), false, "Workflows in the team; a message to one starts it and its output is the reply") : null,
        s.service === "agent" ? field("Agent", "agent", agent) : null,
        s.service === "interactive" ? field("Main agent", "agent", agent, false, "The agent the first chat tab opens with") : null,
        more ? field("Agents", "agents", more, false, "Further agents a new chat tab can be opened with") : null,
        activity ? field("Activity", "activity", activity) : null,
        repository ? field("Repository", "repository", repository, false, "The Git repository the dev pod clones, one of those on the Connectors page") : null,
        repository ? field("Source branch", "source_branch", input(s.source_branch, { placeholder: "Not set", spellcheck: false }), false, "The branch the work starts from") : null,
        repository ? field("Work branch", "work_branch", input(s.work_branch, { placeholder: L.DEFAULT_WORK_BRANCH, spellcheck: false }), false, "The branch the agents commit to, never the source or the default branch; {run} stands for the first 8 characters of the run ID") : null,
        repository ? field("Name", "name", input(s.name, { placeholder: "No name", spellcheck: false }), false, "A later run with the same repository and name continues in the live dev pod of that name; without a name every run gets a new dev pod") : null,
        field("Service", "service", service, false, "agent runs an agent; gate waits for a person; interactive opens a chat session with its agents; route starts the workflow named in the route field of its input; workflow runs another workflow; team lets a lead agent and its members work together; merge waits for a result from every source and forwards them together; batch collects results and forwards them together once it holds its size; devpod starts a dev pod with a clone of a repository on a work branch; command runs one command in the dev pod without a model; devpod-stop deletes the dev pod; any other name is a Temporal task queue"),
        s.service === "merge" ? h("div", { className: "field-pair" },
          field("Capacity", "capacity", digits("capacity", String(L.NODE_PARAMS.merge.capacity)), true, "How many entries may wait for a missing source at the same time; one more fails the run"),
          timeoutField("Seconds an entry may wait for its missing sources, counted from its first result; after that the run fails")) : null,
        s.service === "batch" ? h("div", { className: "field-pair" },
          field("Size", "size", digits("size", "Not set"), true, "How many results the batch collects before it forwards them together"),
          timeoutField("Seconds after the last result until a batch that is not full is forwarded anyway")) : null,
        onReject ? field("Rejections go to", "on_reject", onReject, false, "The earlier step that receives a rejection with its reason and runs again, with the steps between it and the gate; the gate then opens again. A team gets the reason as a message to its lead") : null,
        onReject && s.on_reject ? field("Max rejections", "max_rejections", digits("max_rejections", "5"), true, "How many rejections go back; one more ends the run as rejected") : null,
        // A command runs for at most 10 minutes; a failing one fails the run unless it may fail.
        command ? field("Timeout", "timeout", input(s.timeout, { placeholder: "10m", size: 4, maxLength: 6 }), true, "How long the command may run, such as 30s or 5m; 10m at most") : null,
        allowFailure ? h("div", { className: "field check-field" },
          h("label", { htmlFor: fid("allow_failure"), title: "Ticked, the run goes on when the command ends with an exit code other than 0; unticked, it fails the run" }, allowFailure, "Allow failure")) : null,
        // Timeout and Retries stay together at the right, also when the row wraps. Gates and routes
        // run no activity, so neither would do anything there.
        s.service === "team" ? h("div", { className: "field-pair" },
          field("Timeout", "timeout", input(s.timeout, { placeholder: "15m", size: 4, maxLength: 6 }), true, "How long one turn of a member may take, such as 5m or 15m"),
          field("Max turns", "max_turns", input(s.max_turns ?? "", { placeholder: "40", size: 4, maxLength: 4, inputMode: "numeric" }), true, "How many turns the members may take together before the team fails")) : null,
        s.service === "gate" || s.service === "route" || s.service === "interactive" || s.service === "workflow" || s.service === "team" || node || L.isPodKind(s) ? null : h("div", { className: "field-pair" },
          field("Timeout", "timeout", input(s.timeout, { placeholder: s.service === "agent" ? "2m" : "10m", size: 4, maxLength: 6 }), true, "How long one attempt may take, such as 30s or 10m"),
          field("Retries", "attempts", input(s.max_attempts ? s.max_attempts - 1 : "", { type: "number", min: 0, max: 99, placeholder: "0" }), true, "How often a failed attempt is retried"))),
      // A gate's question gets its own full-width row.
      question ? field("Question", "question", question) : null,
      // A command gets its own full-width row, like a question; so do a dev pod's protected paths and rules.
      command ? field("Command", "command", command, false, "The command the step runs in the dev pod, in /workspace; its exit code decides the step") : null,
      protectedPaths ? h("div", { className: "field-wide" }, field("Protected paths", "protected", protectedPaths, false, "Files and folders the agent cannot change, one glob per line, such as tests/** or verify.sh")) : null,
      s.service === "devpod" ? rulesEditor(s) : null);
    if (allowFailure) { allowFailure.id = fid("allow_failure"); allowFailure.dataset.field = "allow_failure"; }
    // Name fields are as wide as their text, also while typing.
    const one = question || command; // the one sentence or line that wraps and grows
    const fitted = [id, agent, activity, service, nested, nestedVersion, lead, onReject, repository, ...["source_branch", "work_branch", "name"].map((name) => form.querySelector(`[data-field="${name}"]`))].filter(Boolean);
    const grow = (el) => { el.style.height = "auto"; el.style.height = `${el.scrollHeight + 2}px`; };
    requestAnimationFrame(() => { for (const el of fitted) if (el.isConnected) fitWidth(el); if (one?.isConnected) grow(one); });
    form.addEventListener("input", (ev) => { if (fitted.includes(ev.target)) fitWidth(ev.target); if (ev.target === one) grow(one); });
    form.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter") return;
      if (ev.target === one) ev.preventDefault(); // the question stays one paragraph; Enter finishes it like the other fields
      if (ev.target.tagName === "INPUT" || ev.target === one) ev.target.blur();
    });
    form.addEventListener("change", (ev) => {
      const name = ev.target.dataset.field;
      const current = step(s.id);
      if (!name || !current || !stepFields[name]) return;
      stepFields[name](current, ev.target.type === "checkbox" ? ev.target.checked : ev.target.value.trim());
      markDirty();
      refreshAfterEdit();
    });
    return form;
  }

  // agentsPicker is the trigger of the further-agents choice: it names how many are chosen and
  // opens a list of every agent version with checkboxes; Apply stores the choice.
  function agentsPicker(s) {
    const chosen = (s.agents || []).filter((ref) => ref !== s.agent);
    const gone = (ref) => state.agentsLoaded && !state.agentRefs.includes(ref);
    const trigger = h("button", { type: "button", className: "multi-select-trigger", "aria-haspopup": "dialog" },
      chosen.length ? `${chosen.length} agent${chosen.length === 1 ? "" : "s"} selected` : "No agents");
    if (chosen.length) trigger.title = chosen.map((ref) => (gone(ref) ? `${ref} (missing)` : ref)).join("\n");
    trigger.onclick = () => {
      const selected = new Set(chosen);
      const refs = [...new Set([...chosen, ...state.agentRefs])].filter((ref) => ref !== s.agent);
      const apply = h("button", { type: "button", className: "primary" }, "Apply");
      const cancel = h("button", { type: "button" }, "Cancel");
      const list = h("div", { className: "agent-tool-picker-list", role: "group", "aria-label": "Agents" }, ...refs.map((ref) => {
        const check = h("input", { type: "checkbox", value: ref, checked: selected.has(ref) });
        check.addEventListener("change", () => { if (check.checked) selected.add(ref); else selected.delete(ref); });
        return h("label", { className: "agent-tool-picker-item" }, check, h("span", { className: "agent-tool-picker-name" }, gone(ref) ? `${ref} (missing)` : ref));
      }));
      const popover = openPopover(trigger, h("div", {},
        h("div", { className: "agent-policy-popover-list" }, refs.length ? list : h("p", { className: "agent-tool-picker-status", role: "status" }, "No other agents")),
        h("div", { className: "picker-actions" }, cancel, apply)));
      cancel.onclick = () => popover.close();
      apply.onclick = () => {
        popover.close();
        const current = step(s.id);
        if (!current) return;
        stepFields.agents(current, refs.filter((ref) => selected.has(ref)));
        markDirty();
        refreshAfterEdit();
      };
    };
    return trigger;
  }

  // ---- Team and workflow nodes ----

  // applyTeam stores a team's lead and members and drops the channels of members that left.
  function applyTeam(s, next) {
    for (const key of ["agent", "agents", "workflows"]) {
      const value = next[key];
      if (Array.isArray(value) ? value.length : value) s[key] = value; else delete s[key];
    }
    const channels = L.pruneChannels(s);
    if (channels) s.channels = channels; else delete s.channels;
    // A member that left takes its place in the panel with it.
    if (s.member_positions) {
      const names = new Set(L.teamMembers(s).map((m) => m.name));
      for (const name of Object.keys(s.member_positions)) if (!names.has(name)) delete s.member_positions[name];
      if (!Object.keys(s.member_positions).length) delete s.member_positions;
    }
  }

  // membersPicker chooses a team's further agents or its workflows, like agentsPicker. A choice
  // that would give two members the same name is refused and stays open.
  function membersPicker(s, key) {
    const agents = key === "agents";
    const noun = agents ? "agent" : "workflow";
    const chosen = (s[key] || []).filter((ref) => !agents || ref !== s.agent);
    const gone = (ref) => (agents ? state.agentsLoaded && !state.agentRefs.includes(ref) : missingWorkflow(ref));
    const label = (ref) => `${agents ? ref : refLabel(ref)}${gone(ref) ? " (missing)" : ""}`;
    const trigger = h("button", { type: "button", className: "multi-select-trigger", "aria-haspopup": "dialog" },
      chosen.length ? `${chosen.length} ${noun}${chosen.length === 1 ? "" : "s"} selected` : `No ${noun}s`);
    if (chosen.length) trigger.title = chosen.map(label).join("\n");
    trigger.onclick = () => {
      const selected = new Set(chosen);
      // Workflows are offered pinned to their newest version; a chosen one keeps the version it has.
      const offered = agents ? state.agentRefs.filter((ref) => ref !== s.agent)
        : nestable().filter((name) => !chosen.some((ref) => WorkflowRef.nameOf(ref) === name)).map(pinNewest);
      const refs = [...new Set([...chosen, ...offered])];
      const apply = h("button", { type: "button", className: "primary" }, "Apply");
      const cancel = h("button", { type: "button" }, "Cancel");
      const list = h("div", { className: "agent-tool-picker-list", role: "group", "aria-label": agents ? "Agents" : "Workflows" }, ...refs.map((ref) => {
        const check = h("input", { type: "checkbox", value: ref, checked: selected.has(ref) });
        check.addEventListener("change", () => { if (check.checked) selected.add(ref); else selected.delete(ref); });
        return h("label", { className: "agent-tool-picker-item" }, check, h("span", { className: "agent-tool-picker-name" }, label(ref)));
      }));
      const popover = openPopover(trigger, h("div", {},
        h("div", { className: "agent-policy-popover-list" }, refs.length ? list : h("p", { className: "agent-tool-picker-status", role: "status" }, `No other ${noun}s`)),
        h("div", { className: "picker-actions" }, cancel, apply)));
      cancel.onclick = () => popover.close();
      apply.onclick = () => {
        const current = step(s.id);
        if (!current) { popover.close(); return; }
        const next = { ...current, [key]: refs.filter((ref) => selected.has(ref)) };
        const problem = L.teamProblems({ ...next, channels: undefined })[0];
        if (problem) { setStatus(problem, "error"); return; }
        popover.close();
        applyTeam(current, next);
        markDirty();
        refreshAfterEdit();
      };
    };
    return trigger;
  }

  // The member picked in the team panel; picking a second one adds or removes their channel.
  // Make lead and Remove end the pick, so the next click picks a member instead of drawing a channel.
  let teamPick = null; // { step, name }
  let justDragged = false; // set for the click that ends a drag of a member tile
  // A team shows in the node panel and, opened by a double-click, on the canvas: a pick shows in
  // both, and the focus returns to the one last used.
  let teamHost = "inspector";
  const focusMember = (name) => [...$(teamHost).querySelectorAll(".team-member")].find((b) => b.dataset.member === name)?.focus();
  const renderTeams = () => { renderInspector(); if (state.teamOpen) renderCanvas(); };

  // teamPanel shows a team's members as tiles on a ring, the lead at the top, with the channels
  // as lines between them. Dashed lines are the default: the lead has a channel to every member.
  function teamPanel(s) {
    const members = L.teamMembers(s);
    const { pairs, isDefault } = L.teamChannels(s);
    const layout = L.teamLayout(members.length);
    // A member dragged to a place of its own stays there (member_positions, by name: x in percent of
    // the width, y in px); the others keep their place on the ring.
    const placed = (m) => { const p = s.member_positions?.[m.name]; return p && Number.isFinite(p.x) && Number.isFinite(p.y) ? p : null; };
    const at = new Map(members.map((m, i) => [m.name, placed(m) || layout.points[i]]));
    const height = Math.max(layout.height, ...[...at.values()].map((p) => Math.round(p.y + L.MEMBER_H / 2 + 4)));
    const segments = []; // { a, b, els }: the lines of a channel, moved along while a member is dragged
    if (teamPick && (teamPick.step !== s.id || !at.has(teamPick.name))) teamPick = null;
    const picked = teamPick ? members.find((m) => m.name === teamPick.name) : null;
    const edit = (change, undoWord = "") => {
      const run = () => {
        const current = step(s.id);
        if (!current) return;
        change(current);
        markDirty();
        render();
      };
      if (undoWord) withUndo(undoWord, run); else run();
    };
    const setChannels = (current, channels) => { if (channels) current.channels = channels; else delete current.channels; };
    const toggle = (a, b) => {
      const exists = pairs.some(([x, y]) => L.channelId(x, y) === L.channelId(a, b));
      teamPick = null;
      const problem = !exists && L.channelProblem(s, a, b);
      if (problem) { renderTeams(); setStatus(problem, "error"); focusMember(b); return; }
      edit((current) => setChannels(current, L.toggleChannel(current, a, b)), exists ? "Removed" : "");
      focusMember(b);
    };
    const makeLead = (m) => { teamPick = null; edit((current) => applyTeam(current, { ...current, ...L.setLead(current, m.ref) })); focusMember(m.name); };
    const remove = (m) => { teamPick = null; edit((current) => applyTeam(current, { ...current, ...L.removeMember(current, m.name) }), "Removed"); };
    const leadBlocker = (m) => (m.lead ? "Leads the team already" : m.kind === "workflow" ? "A workflow cannot lead a team" : "");
    const removeBlocker = (m) => (m.lead ? "A team needs its lead agent; Lead agent replaces it" : "");

    const lines = el("svg", { class: "team-lines", width: "100%", height, "aria-hidden": "true" });
    const tips = []; // { from, to, line }: an arrow tip on the line from one member, at the tile of the other
    const tip = el("marker", { id: "team-arrow", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto" }, el("defs", {}, lines));
    el("path", { d: "M0,0 L10,5 L0,10 z", class: "team-channel-tip" }, tip);
    for (const pair of pairs) {
      const [a, b] = pair;
      const p = at.get(a), q = at.get(b);
      if (!p || !q) continue; // named in teamProblems below
      const sender = L.channelSender(pair);
      const says = sender ? `from ${a} to ${b} only` : `between ${a} and ${b}, both directions`;
      const g = el("g", {}, lines);
      el("title", {}, g).textContent = `${isDefault ? "Default channel" : "Channel"} ${says}; a click removes it, a right-click changes its direction`;
      const ends = { x1: `${p.x}%`, y1: p.y, x2: `${q.x}%`, y2: q.y };
      segments.push({ a, b, els: [el("line", { ...ends, class: "team-channel-hit" }, g), el("line", { ...ends, class: `team-channel${isDefault ? " default" : ""}` }, g)] });
      // An arrow tip touches the tile of each member that receives on the channel: the receiver of a
      // one-way channel, both members otherwise. placeTips sets it where the line meets that tile.
      const arrow = (from, to) => tips.push({ from, to, line: el("line", { class: "team-channel team-channel-arrow", "marker-end": "url(#team-arrow)", visibility: "hidden" }, g) });
      arrow(a, b);
      if (!sender) arrow(b, a);
      const removeChannel = () => { teamPick = null; edit((current) => setChannels(current, L.toggleChannel(current, a, b)), "Removed"); };
      const direct = (from) => { teamPick = null; edit((current) => setChannels(current, L.setChannelDirection(current, a, b, from))); };
      g.addEventListener("click", removeChannel);
      g.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["Both directions", () => direct(""), "", sender ? `${a} and ${b} both send` : "The channel's direction now", !sender],
        [`${a} → ${b} only`, () => direct(a), "", sender ? "The channel's direction now" : `Only ${a} sends; ${b} cannot answer on this channel`, !!sender],
        [`${b} → ${a} only`, () => direct(b), "", `Only ${b} sends; ${a} cannot answer on this channel`],
        "-",
        ["Remove channel", removeChannel, "danger", `Removes the channel between ${a} and ${b}`],
      ]));
    }

    const tiles = members.map((m, i) => {
      const gone = m.kind === "agent" ? state.agentsLoaded && !state.agentRefs.includes(m.ref) : missingWorkflow(m.ref);
      // A workflow member is named by its workflow; its second line names the pinned version.
      const word = gone ? "Missing" : m.lead ? "Lead" : m.kind === "workflow" ? `Workflow ${L.versionWord(m.ref)}`.trim() : "";
      const newer = m.kind === "workflow" && !gone ? pinNewest(WorkflowRef.nameOf(m.ref)) : "";
      const on = picked === m;
      const tile = h("button", {
        type: "button", className: `team-member${on ? " picked" : ""}${m.lead ? " lead" : ""}${gone ? " missing" : ""}`,
        "aria-pressed": String(on), dataset: { member: m.name },
        "aria-label": `${m.name}${word ? `, ${word.toLowerCase()}` : ""}`,
        title: L.memberTip(m, picked, pairs),
        style: `left:${at.get(m.name).x}%;top:${at.get(m.name).y}px`,
      }, h("span", { className: "team-member-name" }, m.name), word ? h("span", { className: "team-member-kind" }, word) : null);
      tile.addEventListener("focusin", () => { teamHost = tile.closest("#team-canvas") ? "team-canvas" : "inspector"; });
      // The port under the tile: a drag from it onto another member draws a channel between the two.
      const port = h("span", { className: "team-port", title: `Drag onto another member to draw a channel from ${m.name}` });
      tile.append(port);
      let draft = null; // the line that follows the pointer while a channel is drawn
      const pointAt = (ev) => { const box = lines.getBoundingClientRect(); return { x: ev.clientX - box.left, y: ev.clientY - box.top }; };
      port.addEventListener("pointerdown", (ev) => {
        if (ev.button !== 0) return;
        ev.stopPropagation(); // the tile does not move
        port.setPointerCapture?.(ev.pointerId);
        const box = lines.getBoundingClientRect(), r = tile.getBoundingClientRect(), p = pointAt(ev);
        draft = el("line", { class: "team-channel team-channel-draft", x1: r.left + r.width / 2 - box.left, y1: r.bottom - box.top, x2: p.x, y2: p.y }, lines);
      });
      port.addEventListener("pointermove", (ev) => {
        if (!draft) return;
        const p = pointAt(ev);
        draft.setAttribute("x2", p.x);
        draft.setAttribute("y2", p.y);
      });
      const endDraft = (ev, add) => {
        if (!draft) return;
        draft.remove();
        draft = null;
        if (!add) return;
        const target = document.elementFromPoint(ev.clientX, ev.clientY)?.closest(".team-member");
        const to = target && target.parentElement === tile.parentElement ? target.dataset.member : "";
        if (!to || to === m.name) return;
        if (pairs.some(([x, y]) => L.channelId(x, y) === L.channelId(m.name, to))) { setStatus(`${m.name} and ${to} have a channel already`); return; }
        toggle(m.name, to);
      };
      port.addEventListener("pointerup", (ev) => endDraft(ev, true));
      port.addEventListener("pointercancel", (ev) => endDraft(ev, false));
      port.addEventListener("click", (ev) => ev.stopPropagation()); // a click on the port picks nothing
      // A drag moves the member; the tile and its channel lines follow, and the place is stored on release.
      let move = null; // { x, y, dragged, pos }
      tile.addEventListener("pointerdown", (ev) => { if (ev.button === 0) { move = { x: ev.clientX, y: ev.clientY, dragged: false, pos: null }; tile.setPointerCapture?.(ev.pointerId); } });
      tile.addEventListener("pointermove", (ev) => {
        if (!move || (!move.dragged && Math.hypot(ev.clientX - move.x, ev.clientY - move.y) < 5)) return;
        const box = tile.parentElement.getBoundingClientRect();
        if (!box.width) return;
        move.dragged = true;
        const half = (tile.offsetWidth / 2 / box.width) * 100;
        move.pos = {
          x: Math.round(Math.min(100 - half, Math.max(half, ((ev.clientX - box.left) / box.width) * 100)) * 100) / 100,
          y: Math.round(Math.min(height + 96, Math.max(L.MEMBER_H / 2 + 4, ev.clientY - box.top))),
        };
        tile.style.left = `${move.pos.x}%`;
        tile.style.top = `${move.pos.y}px`;
        for (const { a, b, els } of segments) {
          for (const line of els) {
            if (a === m.name) { line.setAttribute("x1", `${move.pos.x}%`); line.setAttribute("y1", move.pos.y); }
            if (b === m.name) { line.setAttribute("x2", `${move.pos.x}%`); line.setAttribute("y2", move.pos.y); }
          }
        }
        placeTips();
      });
      const drop = () => {
        const done = move;
        move = null;
        if (!done?.dragged || !done.pos) return;
        justDragged = true; // the click that ends a drag does not pick the member
        setTimeout(() => { justDragged = false; });
        edit((current) => { current.member_positions = { ...(current.member_positions || {}), [m.name]: done.pos }; });
      };
      tile.addEventListener("pointerup", drop);
      tile.addEventListener("pointercancel", () => { move = null; });
      // A double-click on a workflow member opens that workflow on the canvas.
      tile.addEventListener("dblclick", () => { if (m.kind === "workflow" && !gone) { teamPick = null; openNested(m.ref); } });
      tile.addEventListener("click", () => {
        if (justDragged) return;
        if (picked && !on) { toggle(picked.name, m.name); return; }
        teamPick = on ? null : { step: s.id, name: m.name };
        renderTeams();
        focusMember(m.name);
      });
      tile.addEventListener("keydown", (ev) => {
        if (ev.key !== "Escape" || !teamPick) return;
        teamPick = null;
        renderTeams();
        focusMember(m.name);
      });
      tile.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ...(m.kind === "workflow" && !gone ? [["Open workflow", () => { teamPick = null; openNested(m.ref); }, "", `Shows the workflow ${refLabel(m.ref)} on the canvas`]] : []),
        // A member pinned to an older version can move to the newest one; its name and channels stay.
        ...(newer && newer !== m.ref ? [[`Use ${L.versionWord(newer)}`, () => edit((current) => { current.workflows = (current.workflows || []).map((ref) => (ref === m.ref ? newer : ref)); }), "", `Pins ${m.name} to ${L.versionWord(newer)}, its newest version, instead of ${L.versionWord(m.ref) || "the newest at run time"}`]] : []),
        ["Make lead", () => makeLead(m), "", leadBlocker(m) || `Makes ${m.name} the lead agent; ${members[0].name} stays a member`, !!leadBlocker(m)],
        ["Reset layout", () => edit((current) => { delete current.member_positions; }), "", s.member_positions ? "Puts every member back on the ring" : "No member has been moved", !s.member_positions],
        "-",
        ["Remove member", () => remove(m), "danger", removeBlocker(m) || `Removes ${m.name} and its channels from the team`, !!removeBlocker(m)],
      ]));
      return tile;
    });

    // The lines end at the tile centres, x in percent, so where a line meets a tile is known only
    // once the panel has its width: the tips are placed then, and again whenever the width changes.
    const placeTips = () => {
      const box = lines.getBoundingClientRect();
      if (!box.width) return;
      const rect = new Map(tiles.map((t) => [t.dataset.member, t.getBoundingClientRect()]));
      for (const { from, to, line } of tips) {
        const f = rect.get(from), r = rect.get(to);
        if (!f || !r) continue;
        const end = { x: r.left + r.width / 2 - box.left, y: r.top + r.height / 2 - box.top };
        const dx = f.left + f.width / 2 - box.left - end.x, dy = f.top + f.height / 2 - box.top - end.y;
        const length = Math.hypot(dx, dy);
        if (!length) continue;
        // The share of the line that lies under the receiving tile, then 12 px of line for the tip.
        const edge = Math.min(dx ? (r.width / 2 + 2) / Math.abs(dx) : Infinity, dy ? (r.height / 2 + 2) / Math.abs(dy) : Infinity);
        const at = (t) => ({ x: end.x + dx * t, y: end.y + dy * t });
        const head = at(edge), tail = at(edge + 12 / length);
        for (const [k, v] of Object.entries({ x1: tail.x, y1: tail.y, x2: head.x, y2: head.y })) line.setAttribute(k, v);
        line.removeAttribute("visibility");
      }
    };
    if (typeof ResizeObserver === "function") new ResizeObserver(placeTips).observe(lines);

    // "+" adds a member: the agents that are not in the team yet, then the workflows that may be nested.
    const addMember = iconButton("plus", "Add member", () => {});
    addMember.addEventListener("click", (ev) => {
      const add = (key, ref) => edit((current) => {
        const next = { ...current, [key]: [...(current[key] || []), ref] };
        const problem = L.teamProblems({ ...next, channels: undefined })[0];
        if (problem) { setStatus(problem, "error"); return; }
        applyTeam(current, next);
      });
      const refs = new Set(members.map((m) => m.ref));
      const agents = state.agentRefs.filter((ref) => !refs.has(ref)).map((ref) => [ref, () => add("agents", ref), "", `Adds the agent ${ref} to the team`]);
      // A workflow joins pinned to its newest version, once: a member is named by its workflow.
      const flows = nestable().filter((name) => !members.some((m) => m.kind === "workflow" && WorkflowRef.nameOf(m.ref) === name)).map(pinNewest)
        .map((ref) => [`${refLabel(ref)} (workflow)`, () => add("workflows", ref), "", `Adds the workflow ${refLabel(ref)} as a member`]);
      const items = [...agents, ...(agents.length && flows.length ? ["-"] : []), ...flows];
      openMenu(ev, items.length ? items : [["Nothing left to add", () => {}, "", "Every agent and workflow is a member already", true]]);
    });

    const problems = L.teamProblems(s);
    return h("div", { className: "team-panel" },
      h("div", { className: "file-head" },
        h("h3", { className: "section-title" }, "Members and channels"),
        h("span", { className: "team-channel-state" }, isDefault ? "Default channels" : `${pairs.length} channel${pairs.length === 1 ? "" : "s"}`),
        addMember),
      members.length
        ? h("div", { className: "team-ring", role: "group", "aria-label": "Team members", style: `height:${height}px` }, lines, ...tiles)
        : null,
      ...problems.map((p) => h("p", { className: "error-text team-problem" }, p)));
  }

  // nestedCard lists the steps of the workflow version a workflow node runs, with a link that opens it.
  function nestedCard(s) {
    if (!s.workflow) return null;
    const found = pinnedDef(s.workflow);
    if (found.status === "missing") return h("div", { className: "agent-card" }, h("p", { className: "error-text" }, `${refLabel(s.workflow)} no longer exists`));
    if (found.status === "loading") return h("div", { className: "agent-card" });
    if (found.status === "error") return h("div", { className: "agent-card" }, h("p", { className: "error-text" }, `Cannot load ${refLabel(s.workflow)}: ${found.error}`));
    const target = found.def;
    const { name, version } = WorkflowRef.parse(s.workflow);
    const older = version && version !== newestOf(name) ? version : 0; // the hash names a version only when it is not the newest
    const link = h("a", { href: L.formatHash({ workflow: name, version: older, path: [...state.path, here()] }) }, "Open workflow");
    return h("div", { className: "agent-card" },
      h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Steps"), link),
      h("ul", { className: "nested-steps" }, ...target.steps.map((x) => h("li", {},
        h("span", { className: "nested-step-name" }, titleCase(x.id)), h("span", { className: "nested-step-kind" }, typeName(x))))));
  }

  // refreshAfterEdit redraws after the browser has moved focus (Tab to the next field),
  // then focuses the same field in the same place, so typing is not interrupted.
  function refreshAfterEdit() {
    setTimeout(() => {
      const active = document.activeElement;
      const field = active?.closest?.("#inspector") ? active.dataset?.field : null;
      const range = field && typeof active.selectionStart === "number" ? [active.selectionStart, active.selectionEnd] : null;
      render();
      if (!field) return;
      const next = $("inspector").querySelector(`[data-field="${field}"]`);
      next?.focus();
      if (range && next?.setSelectionRange) try { next.setSelectionRange(...range); } catch { /* number inputs */ }
    }, 0);
  }

  // Agent definitions, cached per ref for the session.
  const agentDefs = new Map();

  function agentDef(ref) {
    if (!agentDefs.has(ref)) {
      agentDefs.set(ref, { status: "loading" });
      const [name, version] = ref.split("@");
      api("GET", `/api/agents/agents/${name.split("/").map(encodeURIComponent).join("/")}/${encodeURIComponent(version)}`)
        .then((def) => agentDefs.set(ref, { status: "ok", def }), (e) => agentDefs.set(ref, { status: "error", error: e.message }))
        .then(refreshAfterEdit);
    }
    return agentDefs.get(ref);
  }

  // agentCard shows the agent's files as the Agents page does: the prompt, then the config.
  function agentCard(ref) {
    if (!/^[^@\s]+@[^@\s]+$/.test(ref)) return null;
    // A missing agent is said to be missing, as on its tile, not reported as a failed load.
    if (state.agentsLoaded && !state.agentRefs.includes(ref)) return h("div", { className: "agent-card" }, h("p", { className: "error-text" }, `${ref} no longer exists`));
    const [name, version] = ref.split("@");
    const link = h("a", { href: `#agents/${name}/${version}` }, "View agent");
    const entry = agentDef(ref);
    if (entry.status === "loading") return h("div", { className: "agent-card" });
    if (entry.status === "error") return h("div", { className: "agent-card" }, h("p", { className: "error-text" }, `Cannot load ${ref}: ${entry.error}`));
    const files = entry.def.files || {};
    let config = files["agent.json"] ?? {};
    if (typeof config === "string") { try { config = JSON.parse(config); } catch { /* shown as stored */ } }
    const prompt = typeof files["systemprompt.md"] === "string" ? files["systemprompt.md"] : "";
    const promptView = textView(prompt, { detectJSON: false });
    return h("div", { className: "agent-card" },
      h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "System prompt (systemprompt.md)"), link, promptView.toggle),
      promptView.body,
      h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Config (agent.json)")),
      h("div", { className: "file-block code" }, typeof config === "string" ? config : JSON.stringify(config, null, 2)));
  }

  // The node panel toggle sits in the toggles row with the sidebar toggle.
  function setInspector(open) {
    state.inspectorOpen = open;
    try { localStorage.setItem("factory.inspector", open ? "open" : "closed"); } catch { /* storage unavailable */ }
    renderToggles();
  }
  const inspectorToggle = iconButton("panelRight", "Detail panel", () => { setInspector(!state.inspectorOpen); renderInspector(); if (state.inspectorOpen) revealPanel($("inspector"), true); });
  pageToggles.workflows = [inspectorToggle];
  function renderToggles() {
    inspectorToggle.classList.toggle("on", state.inspectorOpen);
    inspectorToggle.setAttribute("aria-pressed", String(state.inspectorOpen));
  }

  // ---- Interaction on the canvas ----

  function point(ev) {
    const r = $("canvas").getBoundingClientRect();
    return { x: (ev.clientX - r.left - view.ox) / view.scale, y: (ev.clientY - r.top - view.oy) / view.scale };
  }

  // startPan begins a drag of the whole view: from the empty canvas, or anywhere with Shift.
  function startPan(ev) {
    ev.stopPropagation();
    drag = { kind: "pan", x: ev.clientX, y: ev.clientY, ox: view.ox, oy: view.oy };
    $("canvas").classList.add("panning");
  }

  function startMove(ev, s) {
    ev.stopPropagation();
    if (ev.button !== 0) return;
    if (ev.shiftKey) { startPan(ev); return; }
    const p = point(ev);
    drag = { kind: "move", step: s, dx: p.x - s.position.x, dy: p.y - s.position.y, moved: false };
    if (!state.inspectorOpen) setInspector(true);
    if (state.selected !== s.id) { state.selected = s.id; render(); }
  }

  function startConnect(ev, s) {
    ev.stopPropagation();
    if (ev.button !== 0) return;
    if (ev.shiftKey) { startPan(ev); return; }
    drag = { kind: "connect", from: s, line: null };
  }

  // startReject begins a drag from a gate's Rejected port: dropped on an earlier step, it sets
  // where the gate's rejections go.
  function startReject(ev, s) {
    ev.stopPropagation();
    if (ev.button !== 0) return;
    if (ev.shiftKey) { startPan(ev); return; }
    drag = { kind: "reject", from: s, line: null };
  }

  function onPointerMove(ev) {
    if (!drag) return;
    if (drag.kind === "pan") {
      view.touched = true;
      view.ox = drag.ox + ev.clientX - drag.x;
      view.oy = drag.oy + ev.clientY - drag.y;
      renderCanvas();
      return;
    }
    const p = point(ev);
    if (drag.kind === "move") {
      const next = { x: Math.round(p.x - drag.dx), y: Math.round(p.y - drag.dy) };
      if (!drag.moved && Math.hypot(next.x - drag.step.position.x, next.y - drag.step.position.y) < 3) return;
      drag.step.position = next;
      drag.moved = true;
      view.touched = true;
      renderCanvas();
    } else if (drag.kind === "reject") {
      drag.line?.remove();
      const from = { x: drag.from.position.x + TILE_W, y: drag.from.position.y + L.REJECT_PORT_Y };
      drag.line = el("path", { d: `M${from.x},${from.y} L${p.x},${p.y}`, class: "back-edge", "marker-end": "url(#arrow-reject)" }, $("canvas").querySelector(".graph"));
    } else {
      drag.line?.remove();
      drag.line = el("path", { d: edgePath(outPort(drag.from), { x: p.x, y: p.y + 8 }), class: "edge", "marker-end": "url(#arrow)" }, $("canvas").querySelector(".graph"));
    }
  }

  function onPointerUp(ev) {
    if (!drag) return;
    const d = drag;
    drag = null;
    if (d.kind === "pan") { $("canvas").classList.remove("panning"); saveView(workflow().name); return; }
    if (d.kind === "move") {
      if (d.moved) { markDirty(); renderCanvas(); }
      return;
    }
    d.line?.remove();
    const p = point(ev);
    const target = workflow().steps.find((s) => p.x >= s.position.x && p.x <= s.position.x + TILE_W && p.y >= s.position.y && p.y <= s.position.y + TILE_H);
    if (d.kind === "reject") {
      // A drop the orchestrator would refuse is rejected at once, with the reason.
      if (target && target !== d.from) {
        const refusal = L.rejectRefusal(workflow(), d.from, target.id, titleCase);
        if (refusal) setStatus(refusal, "error");
        else { d.from.on_reject = target.id; markDirty(); }
      }
      render();
      return;
    }
    if (target && target !== d.from) {
      const deps = target.depends_on || [];
      if (deps.includes(d.from.id)) setStatus("That dependency already exists", "error");
      else if (target.service === "route" && L.isNode(d.from)) setStatus(`A route step cannot follow a ${d.from.service}`, "error");
      else if (target.service === "route" && deps.length) setStatus("A route step has only one dependency", "error");
      else if (target.service === "workflow" && deps.length) setStatus("A workflow step has only one dependency", "error");
      else if (reaches(target.id, d.from.id)) setStatus("That dependency would create a cycle", "error");
      else { target.depends_on = [...deps, d.from.id]; markDirty(); }
    }
    render();
  }

  // reaches reports whether step `to` depends, directly or transitively, on step `from`.
  function reaches(from, to) {
    const seen = new Set();
    const stack = [to];
    while (stack.length) {
      const id = stack.pop();
      if (id === from) return true;
      if (seen.has(id)) continue;
      seen.add(id);
      stack.push(...(step(id)?.depends_on || []));
    }
    return false;
  }

  // ---- Menus ----

  // Escape closes the menu.
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape" || activeTab !== "workflows") return;
    if (!menu.hidden) closeMenu();
  });

  function stepActions(s) {
    const items = [["Duplicate step", () => duplicateStep(s), "", "Copies the step settings under a new ID, without dependencies"]];
    const ref = usesAgent(s) && s.agent ? s.agent.split("@") : null;
    if (ref?.length === 2 && ref[0] && ref[1] && agentDef(s.agent).status !== "error") items.push(["View agent", () => { location.hash = `#agents/${ref[0]}/${ref[1]}`; }, "", `Opens ${s.agent} in Agents`]);
    if (s.service === "workflow" && s.workflow && !missingWorkflow(s.workflow)) items.push(["Open workflow", () => openNested(s.workflow), "", `Opens ${refLabel(s.workflow)}, the workflow version this step runs`]);
    if ((s.depends_on || []).length || workflow().steps.some((o) => (o.depends_on || []).includes(s.id))) {
      items.push("-", ["Remove all dependencies", () => removeAllDependencies(s), "danger", `Removes every arrow into and out of ${titleCase(s.id)}`]);
    }
    items.push("-", ["Delete step", () => deleteStep(s.id), "danger", `Deletes ${titleCase(s.id)} and its arrows`]);
    return items;
  }

  function canvasActions() {
    const w = workflow();
    const items = [];
    if (w) {
      const blocked = runBlocker(); // a run uses a stored version: a draft and unsaved edits are saved first
      items.push(
        ["Run workflow…", openInputs, "", blocked || `Opens Select Inputs to start runs of v${shownVersion()}`, !!blocked],
        "-",
        ["Add agent", () => addStep("agent"), "", "Adds an agent node without connections"],
        ["Add human gate", () => addStep("gate"), "", "Adds a human approval node without connections"],
        ["Add interactive session", () => addStep("interactive"), "", "Adds a chat session node without connections"],
        ["Add workflow", () => addStep("workflow"), "", "Adds a child workflow node without connections"],
        ["Add team", () => addStep("team"), "", "Adds a team node without connections"],
        ["Add router", () => addStep("route"), "", "Adds a routing node without connections"],
        ["Add service", () => addStep("service"), "", "Adds a custom service node without connections"],
        ["Add merge", () => addStep("merge"), "", "Adds a merge node: it waits for a result from every source and forwards them together"],
        ["Add batch", () => addStep("batch"), "", "Adds a batch node: it collects results and forwards them together once it holds its size"],
        ["Add dev pod", () => addStep("devpod"), "", "Adds a dev pod node: it starts a sandbox with a clone of a repository on a work branch"],
        ["Add command", () => addStep("command"), "", "Adds a command node: it runs one command in the dev pod, without a model"],
        ["Add stop dev pod", () => addStep("devpod-stop"), "", "Adds a node that deletes the dev pod"],
        ["Auto-arrange", autoArrange, "", "Discards the saved positions and lays the steps out top to bottom"],
        "-",
      );
    }
    if (w) items.push(
      ["Rename workflow…", () => renameWorkflow(), "", "Renames it with every version, or moves it into another group"],
      ["Duplicate workflow", () => duplicateWorkflow(), "", `Copies ${titleCase(w.name)} with all steps and positions`],
      "-",
      ["Delete workflow", () => deleteWorkflow(), "danger", `Deletes ${titleCase(w.name)} with all its versions`],
      "-",
    );
    items.push(...newItems(w ? groupOf(w.name) : ""));
    return items;
  }

  // ---- Edits ----

  // withUndo runs a destructive edit and keeps Undo for Cmd/Ctrl+Z. Undo puts the steps back as
  // they were; like the edit, it stays unsaved until Save.
  function withUndo(message, edit) {
    const of = workflow();
    const before = structuredClone(of.steps);
    const selected = state.selected;
    edit();
    setStatus(message, "", { label: "Undo", run: () => {
      if (workflow() !== of) return; // another workflow or version is on the canvas
      of.steps = before;
      state.selected = selected;
      markDirty();
      render();
    } });
  }

  function removeDependency(s, from) {
    withUndo("Removed", () => {
      s.depends_on = (s.depends_on || []).filter((x) => x !== from);
      markDirty();
      render();
    });
  }

  // removeBackEdge ends a gate's back edge: a rejection ends the run again.
  function removeBackEdge(s) {
    withUndo("Removed", () => {
      delete s.on_reject;
      delete s.max_rejections;
      markDirty();
      render();
    });
  }

  function removeAllDependencies(s) {
    delete s.depends_on;
    for (const o of workflow().steps) o.depends_on = (o.depends_on || []).filter((d) => d !== s.id);
    markDirty();
    render();
  }

  function duplicateStep(s) {
    let id = `${s.id}-copy`;
    for (let n = 2; step(id); n++) id = `${s.id}-copy-${n}`;
    const copy = { id, service: s.service };
    for (const k of ["activity", "agent", "agents", "question", "timeout", "max_attempts", "workflow", "workflows", "channels", "max_turns", "member_positions", "capacity", "size", "input_timeout",
      "repository", "source_branch", "work_branch", "name", "protected", "rules", "command", "allow_failure"]) if (s[k] !== undefined) copy[k] = structuredClone(s[k]);
    copy.position = { x: s.position.x + 40, y: s.position.y + 40 };
    workflow().steps.push(copy);
    state.selected = id;
    markDirty();
    render();
    setSaveState("Duplicated");
  }

  function deleteStep(id) {
    withUndo("Deleted", () => {
      const w = workflow();
      w.steps = w.steps.filter((s) => s.id !== id);
      for (const s of w.steps) s.depends_on = (s.depends_on || []).filter((d) => d !== id);
      if (state.selected === id) state.selected = null;
      markDirty();
      render();
    });
  }

  // addStep adds an agent step, or a merge or batch node when one is named.
  function addStep(service = "agent") {
    const w = workflow();
    const word = STEP_WORD[service] || service;
    let n = 1; // the lowest free number, so the first new step is Step 1 whatever the others are called
    while (w.steps.some((s) => sameName(s.id, `${word}-${n}`))) n++;
    const added = { id: `${word}-${n}`, service, position: freeSlot(w, 0) };
    if (["agent", "interactive", "team"].includes(service) && state.agentRefs[0]) added.agent = state.agentRefs[0];
    if (service === "devpod") podDefaults(added);
    w.steps.push(added);
    state.selected = added.id;
    markDirty();
    render();
  }

  function autoArrange() {
    withUndo("Arranged", () => {
      const w = workflow();
      for (const s of w.steps) delete s.position;
      autoLayout(w);
      try { localStorage.removeItem(viewKey(w.name)); } catch { /* storage unavailable */ }
      resetView();
      markDirty();
      render();
    });
  }

  // newWorkflow asks for a group and a name, then starts a draft: it is on the page only, and its
  // first Save stores it as v1.
  async function newWorkflow(group = "") {
    if (!(await confirmLeave())) return;
    const inGroup = (leaf) => [group, leaf].filter(Boolean).join("/");
    let n = 1; // the lowest free number in the group, as for steps
    while (state.workflows.some((w) => !isDraft(w) && sameName(w.name, inGroup(`workflow-${n}`)))) n++;
    nameDialog("New workflow", "Create", inGroup(`workflow-${n}`), (fields) => {
      const name = fields.check();
      if (!name) return false;
      // There is one draft at a time: an earlier one that was left without a step gives way.
      state.workflows = state.workflows.filter((w) => !isDraft(w));
      state.workflows.push({ name, steps: [] });
      state.current = state.workflows.length - 1;
      state.selected = null;
      state.path = [];
      showDefinition(entry());
      dropUndo();
      resetView();
      render();
      return true;
    });
  }

  // duplicateWorkflow stores a copy at once, as v1 of a new name beside the original (in its
  // group), and opens it. The copy of the workflow on the canvas holds what the canvas shows;
  // any other, its newest version.
  async function duplicateWorkflow(i = state.current) {
    const e = state.workflows[i];
    if (!e) return;
    if (descriptionActionBlocked("duplicated")) return;
    const source = i === state.current && state.edit ? state.edit : e;
    let name = `${e.name}-copy`;
    for (let n = 2; state.workflows.some((x) => sameName(x.name, name)); n++) name = `${e.name}-copy-${n}`;
    let copy;
    try { copy = withVersions(await api("POST", `${API}/workflows/${encodeURIComponent(name)}/versions`, { ...L.storable(structuredClone(source)), name }, creatorHeaders())); }
    catch (err) { setStatus(`Not duplicated: ${namedError(err)}`, "error"); return; }
    state.workflows.push(copy);
    setSaveState("Duplicated");
    renderList();
    openTarget({ name: copy.name }); // unsaved edits of the open workflow are asked about first
  }

  // deleteWorkflow removes a workflow with all its versions. It cannot be undone, so it asks first.
  async function deleteWorkflow(i = state.current) {
    const e = state.workflows[i];
    if (!e) return;
    if (descriptionActionBlocked("deleted")) return;
    const draft = isDraft(e);
    if (!(await confirmAction(draft ? `Delete ${titleCase(e.name)}?` : `Delete ${titleCase(e.name)} with all its versions?`, "Delete"))) return;
    if (!draft) {
      try { await api("DELETE", `${API}/workflows/${encodeURIComponent(e.name)}`, undefined, creatorHeaders()); }
      catch (err) { setStatus(`Not deleted: ${namedError(err)}`, "error"); return; }
    }
    removeWorkflows([e]);
    setSaveState("Deleted");
  }

  // deleteGroup deletes a group with every workflow in it. It cannot be undone, so it asks first.
  async function deleteGroup(group) {
    if (descriptionActionBlocked("deleted")) return;
    const inside = state.workflows.filter((w) => w.name.startsWith(`${group}/`));
    if (inside.length && !(await confirmAction(`Delete ${titleCase(group)} with its ${inside.length} workflow${inside.length === 1 ? "" : "s"} and all their versions?`, "Delete"))) return;
    const gone = [];
    try {
      for (const e of inside) {
        if (!isDraft(e)) await api("DELETE", `${API}/workflows/${encodeURIComponent(e.name)}`, undefined, creatorHeaders());
        gone.push(e);
      }
    } catch (err) { setStatus(`Not deleted: ${namedError(err)}`, "error"); removeWorkflows(gone); return; }
    for (const g of [...groups]) if (g === group || g.startsWith(`${group}/`)) groups.delete(g);
    removeWorkflows(gone);
    setSaveState("Deleted");
  }

  // removeWorkflows takes deleted workflows off the page. When the one on the canvas is among
  // them, its neighbour in the tree takes its place: the next one, else the previous one, in the
  // same group when there is one; a deleted group hands over to the workflow after it.
  function removeWorkflows(gone) {
    if (!gone.length) { render(); return; }
    const order = treeOrder(), names = new Set(gone.map((e) => e.name));
    const first = order.findIndex((n) => names.has(n)), last = order.findLastIndex((n) => names.has(n));
    const after = order.slice(last + 1).filter((n) => !names.has(n)), before = order.slice(0, Math.max(0, first)).filter((n) => !names.has(n)).reverse();
    const same = gone.length === 1 ? (n) => groupOf(n) === groupOf(gone[0].name) : () => false;
    const next = after.find(same) || before.find(same) || after[0] || before[0] || "";
    const shown = entry();
    state.workflows = state.workflows.filter((w) => !gone.includes(w));
    versionCache.clear();
    if (shown && gone.includes(shown)) {
      state.current = indexOf(next);
      state.selected = null;
      state.path = [];
      showDefinition(entry() || null);
      dropUndo();
      resetView();
    } else state.current = state.workflows.indexOf(shown);
    render();
  }

  // ---- Deep links: #workflows/<workflow>/<step>; selecting pushes history so Back works. ----

  // The page without a workflow in its hash reopens the workflow and step last shown, as Agents
  // and Blobstore reopen theirs; one that is gone since gives way to the current one without a notice.
  const LAST_KEY = "factory.workflows.last";

  // currentHash names what the canvas shows; the version only when it is not the newest.
  function currentHash() {
    const w = workflow();
    if (!w) return "#workflows";
    const version = shownVersion() !== WorkflowRef.newest(entry()) ? shownVersion() : 0;
    return L.formatHash({ workflow: w.name, step: state.selected || "", version, path: state.path });
  }

  function syncHash() {
    const w = workflow();
    const want = currentHash();
    if (activeTab !== "workflows") return;
    try { if (w) localStorage.setItem(LAST_KEY, want.slice("#workflows".length)); } catch { /* storage unavailable */ }
    if (location.hash === want) return;
    const shown = L.parseHash(location.hash.slice("#workflows".length));
    const sameWorkflow = shown.workflow === (w?.name || "") && shown.version === L.parseHash(want.slice("#workflows".length)).version;
    history[sameWorkflow ? "replaceState" : "pushState"](null, "", want);
  }

  // followHash opens what a hash names: a workflow, optionally a version, a step, and the way
  // there. A workflow that is gone gives way to the one shown, else to the first. When unsaved
  // edits are kept instead, the hash returns to what the canvas shows.
  async function followHash(rest, remembered = false) {
    const target = L.parseHash(rest, state.workflows.map((w) => w.name));
    let name = target.workflow;
    if (!name || !state.workflows.some((w) => w.name === name)) {
      if (name && !remembered) setStatus(`There is no workflow named ${titleCase(name)}`, "error");
      if (state.edit || !state.workflows.length) { render(); return; } // what is shown stays
      name = treeOrder()[0];
      target.version = 0;
    }
    const shown = await openTarget({ name, version: target.version, step: target.step, path: target.path });
    if (!shown && state.edit && activeTab === "workflows") history.replaceState(null, "", currentHash());
  }

  // ---- Wiring ----

  // A drag on the empty canvas pans it; nodes and arrows keep their own drag and click.
  $("canvas").addEventListener("pointerdown", (ev) => {
    if (ev.button !== 0 || ev.target.closest?.(".edge, .edge-hit, .back-edge")) return;
    startPan(ev);
  });
  $("canvas").addEventListener("contextmenu", (ev) => openMenu(ev, canvasActions()));
  // A double-click on a workflow node opens the workflow it runs.
  $("canvas").addEventListener("dblclick", (ev) => {
    const s = step(ev.target.closest?.(".tile")?.getAttribute("data-step"));
    if (s?.service === "workflow" && s.workflow) openNested(s.workflow); // the pinned version
    // A double-click on a team node opens the team on the canvas: its members and channels.
    if (s?.service === "team") { state.teamOpen = { workflow: workflow().name, step: s.id }; state.selected = s.id; render(); }
  });
  // The Service field suggests the kinds this page adds to the list in the page.
  for (const value of ["workflow", "team", ...L.NODE_KINDS, ...L.POD_KINDS]) {
    if (![...$("services").options].some((o) => o.value === value)) $("services").append(h("option", { value }));
  }
  window.addEventListener("pointermove", onPointerMove);
  window.addEventListener("pointerup", onPointerUp);
  window.addEventListener("beforeunload", (ev) => { if (state.dirty || descriptionDirty() || descriptionEdit?.saving) ev.preventDefault(); });

  tabs.workflows = {
    // Opening the page refreshes from the server (unless edits are not saved yet);
    // later hash changes only change the selection.
    show(rest) {
      let last = "";
      if (!rest.replace(/^\/+/, "")) try { last = localStorage.getItem(LAST_KEY) || ""; } catch { /* storage unavailable */ }
      wanted = { rest: rest.replace(/^\/+/, "") ? rest : last, remembered: !!last };
      const fresh = state.stale;
      state.stale = false;
      if (fresh) { loadAgents(); loadRepositories(); }
      // Unsaved edits survive a visit to another page: nothing is loaded over them, and a hash
      // that names another workflow or version asks first.
      if (fresh && !state.dirty && !state.saving && !descriptionDirty() && !descriptionEdit?.saving) { load(); return; }
      if (state.loaded) followHash(wanted.rest, wanted.remembered);
    },
    hide() {
      state.stale = true;
      closeMenu();
    },
  };
  render();
})();
