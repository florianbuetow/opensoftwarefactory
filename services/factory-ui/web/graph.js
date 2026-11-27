"use strict";
// WorkflowGraph draws a workflow as its graph of step tiles and arrows. It attaches no editing:
// the Workflows editor adds moving, connecting and menus through the hooks of drawGraph, and the
// Runs page and Overview draw the same graph read-only, each tile showing its step's state.
// It uses WorkflowLogic (workflows.js) for the tile outlines and arrow routes, when it draws.
const WorkflowGraph = (() => {
  const SVG_NS = "http://www.w3.org/2000/svg";
  const TILE_W = 220, TILE_H = 64;
  const BAND_W = 12; // the side bands of a workflow node
  const TOP = 40, ROW_H = TILE_H + 72, COL_W = TILE_W + 48, GRAPH_MARGIN = 32;
  const BASE_KINDS = ["agent", "gate", "interactive", "route", "workflow", "team", "merge", "batch"];
  const TYPE_ICON = { agent: "agent", gate: "gate", interactive: "chat", route: "workflow", workflow: "workflow" };
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
  // The outline of a tile comes from WorkflowLogic.tileOutline. Icon and text move in by this
  // much where a band or a slanted edge takes the room.
  const TILE_PAD = { workflow: BAND_W, gate: 6, interactive: 8, merge: 12, devpod: 10, command: 8, "devpod-stop": 12 };
  const TRAY_LINE_Y = 8; // a batch's second line, under its top edge
  // A tile's state, by the words of the state chips.
  const STATE_WORD = { running: "Running", done: "Completed", failed: "Failed", waiting: "Waiting", blocked: "Blocked", idle: "Idle" }; // the words of the Steps list, so a tile and its row never disagree

  const L = () => WorkflowLogic;
  const kinds = () => [...BASE_KINDS, ...L().POD_KINDS];
  const kind = (s) => (kinds().includes(s.service) ? s.service : "custom");
  const inPort = (s) => ({ x: s.position.x + TILE_W / 2, y: s.position.y });
  const outPort = (s) => ({ x: s.position.x + TILE_W / 2, y: s.position.y + TILE_H });
  const stepIn = (w, id) => w.steps.find((s) => s.id === id);

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
  // subtitle is a tile's second line where no editor knows which agents and workflows are gone:
  // what the step runs, asks, or holds.
  function subtitle(s) {
    switch (s.service) {
      case "agent": case "interactive": return s.agent || "";
      case "gate": return s.question || "";
      case "route": return "";
      case "merge": case "batch": return L().nodeSubtitle(s);
      case "devpod": case "command": case "devpod-stop": return L().podSubtitle(s);
      case "workflow": return s.workflow ? `${titleCase(WorkflowRef.nameOf(s.workflow))}${L().versionWord(s.workflow) ? ` ${L().versionWord(s.workflow)}` : ""}` : "";
      case "team": return s.agent || "";
      default: return `${s.service || "?"} · ${s.activity || "Process"}`;
    }
  }

  function el(name, attrs = {}, parent) {
    const e = document.createElementNS(SVG_NS, name);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    if (parent) parent.appendChild(e);
    return e;
  }

  const fit = (text, width, px = 6.6) => {
    const max = Math.max(4, Math.floor(width / px));
    return text.length > max ? `${text.slice(0, max - 1)}…` : text;
  };

  // edgePath draws a dependency from an output port down to an input port. It ends 14 px above
  // the port centre, so the arrowhead (about 8 px long) touches the port circle instead of hiding
  // under it. An edge that skips a row would run behind the tiles in between, so it turns aside in
  // the gap under its source, runs down the free channel beside the source column (toward its
  // target; for the same column, left of the leftmost column, else right), and turns back in the
  // gap above its target. Edges that skip more rows run a little further out, so parallel edges
  // stay apart and no line crosses a tile.
  function edgePath(a, b, left = false) {
    const sx = laneX(a, b, left);
    if (sx !== null) {
      const gap = ROW_H - TILE_H;
      return `M${a.x},${a.y} C${a.x},${a.y + gap * 0.7} ${sx},${a.y + gap * 0.2} ${sx},${a.y + gap * 0.85}`
        + ` L${sx},${b.y - gap * 0.85} C${sx},${b.y - gap * 0.2} ${b.x},${b.y - gap * 0.7} ${b.x},${b.y - 14}`;
    }
    // A target more than a column aside runs down into the gap, along it, and down into the
    // target, with rounded turns: never one long diagonal across the gap and its other arrows.
    // Arrows from different columns run along the gap a little apart, so they stay told apart.
    const ty = b.y - 14;
    if (Math.abs(b.x - a.x) > TILE_W && ty - a.y > 40) {
      const lane = ((Math.round(a.x / COL_W) % 3) + 3) % 3 - 1; // -1, 0 or 1
      const y = Math.round((a.y + ty) / 2 + lane * 8), d = b.x > a.x ? 1 : -1, r = Math.min(10, (ty - a.y) / 4);
      return `M${a.x},${a.y} L${a.x},${y - r} Q${a.x},${y} ${a.x + d * r},${y} L${b.x - d * r},${y} Q${b.x},${y} ${b.x},${y + r} L${b.x},${ty}`;
    }
    const dy = Math.max(30, Math.abs(b.y - a.y) / 2);
    return `M${a.x},${a.y} C${a.x},${a.y + dy} ${b.x},${b.y - dy} ${b.x},${ty}`;
  }

  // dependencyPath draws the arrow from a step to one that depends on it: down from port to
  // port, or, for a step beside its source, from side to side (see WorkflowLogic.edgeRoute), so
  // that no arrow runs behind a tile. It ends 9 px before the tile, where the arrowhead touches it.
  function dependencyPath(w, from, s) {
    const route = L().edgeRoute(from.position, s.position, w.steps.filter((o) => o !== from && o !== s && o.position).map((o) => o.position), { w: TILE_W, h: TILE_H });
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

  // backEdge is the route of a gate's back edge to the step its rejections go to, or null for
  // a step without one. A team's outline reaches 8 px past its box, where its stack shows.
  function backEdge(w, s) {
    const target = s.service === "gate" && s.on_reject ? stepIn(w, s.on_reject) : null;
    if (!target?.position || !s.position || target === s) return null;
    return L().backEdgeRoute(s.position, target.position, w.steps.filter((o) => o !== s && o !== target && o.position).map((o) => o.position),
      { w: TILE_W, h: TILE_H }, target.service === "team" ? 8 : 0);
  }

  // inputPos places the input node one row above the first steps, centred over them.
  function inputPos(w) {
    const roots = w.steps.filter((s) => !(s.depends_on || []).length);
    if (!roots.length) return null;
    const minX = Math.min(...roots.map((s) => s.position.x)), maxX = Math.max(...roots.map((s) => s.position.x + TILE_W));
    return { x: Math.round((minX + maxX) / 2 - TILE_W / 2), y: Math.min(...roots.map((s) => s.position.y)) - ROW_H };
  }

  // stepGroupBoxes gives the frame of each step group of w (a labelled part of the graph, see
  // Definition.Groups): the box around its nodes that have a position, 16 px outside them
  // and with room for the label above the top row. A step group inside another one is drawn a
  // little tighter, so the two frames never lie on each other. index is its place in w.groups.
  const GROUP_PAD = 16, GROUP_LABEL = 22;
  function stepGroupBoxes(w) {
    const list = (w?.groups || []).map((b, index) => {
      const at = (b.nodes || []).map((id) => stepIn(w, id)?.position).filter(Boolean);
      if (!at.length) return null;
      const minX = Math.min(...at.map((p) => p.x)), maxX = Math.max(...at.map((p) => p.x + TILE_W));
      const minY = Math.min(...at.map((p) => p.y)), maxY = Math.max(...at.map((p) => p.y + TILE_H));
      return { index, label: b.label || "", nodes: b.nodes || [], x: minX - GROUP_PAD, y: minY - GROUP_PAD - GROUP_LABEL, w: maxX - minX + 2 * GROUP_PAD, h: maxY - minY + 2 * GROUP_PAD + GROUP_LABEL };
    }).filter(Boolean);
    // A frame that holds all the nodes of another, larger one grows by 6 px per frame inside it.
    for (const a of list) {
      const inside = list.filter((b) => b !== a && b.nodes.length < a.nodes.length && b.nodes.every((id) => a.nodes.includes(id))).length;
      // Its label then sits a line above the label of the frame inside it, never on it.
      if (inside) { const d = 6 * inside, up = GROUP_LABEL * inside; a.x -= d; a.y -= d + up; a.w += 2 * d; a.h += 2 * d + up; }
    }
    return list;
  }

  // bounds is the box around everything the graph draws: its tiles, the input node (with input),
  // the lanes of arrows that skip rows, the back edges that run around the tiles, and the
  // frames of its step groups.
  function bounds(w, input = true) {
    const ip = input ? inputPos(w) : null;
    const boxes = [...w.steps.map((s) => ({ ...s.position, w: TILE_W, h: TILE_H })), ...(ip ? [{ ...ip, w: TILE_W, h: TILE_H }] : []), ...stepGroupBoxes(w)];
    for (const s of w.steps) for (const d of s.depends_on || []) {
      const f = stepIn(w, d);
      const x = f ? laneX(outPort(f), inPort(s), bowsLeft(w, f, s)) : null;
      if (x !== null) boxes.push({ x: x - 8, y: s.position.y, w: 16, h: TILE_H });
    }
    for (const s of w.steps) for (const p of backEdge(w, s)?.points || []) boxes.push({ x: p.x - 8, y: p.y - 8, w: 16, h: 16 });
    const minX = Math.min(...boxes.map((b) => b.x)), maxX = Math.max(...boxes.map((b) => b.x + b.w));
    const minY = Math.min(...boxes.map((b) => b.y)), maxY = Math.max(...boxes.map((b) => b.y + b.h));
    return { minX, maxX, minY, maxY };
  }

  // fitView centres the graph in a canvas of the given width, zoomed out to fit (down to
  // minScale); with a height it also fits the graph's height into it.
  function fitView(w, width, { input = true, minScale = 0.5, maxScale = 1, height = 0, margin = GRAPH_MARGIN } = {}) {
    const b = bounds(w, input);
    const graphW = b.maxX - b.minX, graphH = b.maxY - b.minY;
    let scale = Math.min(maxScale, (width - 2 * margin) / graphW);
    if (height) scale = Math.min(scale, (height - 2 * margin) / graphH);
    scale = Math.max(minScale, scale);
    const ox = Math.max(margin, (width - graphW * scale) / 2) - b.minX * scale;
    const oy = (height ? Math.max(margin, (height - graphH * scale) / 2) : margin) - b.minY * scale;
    return { ox, oy, scale, width: graphW * scale + 2 * margin, height: graphH * scale + 2 * margin };
  }

  // drawGraph draws w into svg, replacing what it held, and returns { root, defs }.
  //   view: { ox, oy, scale }, the pan and zoom (default: fitted to the svg's width)
  //   subtitle(s): the second line of a tile; missing(s): why its agent or workflow is gone, or ""
  //   selected: the ID of the selected step
  //   stateOf(id): the state of a step in a run ("running", "done", ...); its tile then shows the
  //     state as a dot and a word on its second line, and its outline in the state colour
  //   input: draw the Input node; inputEdge(s): whether an arrow runs from it to the root step s
  //   edgeTitle(s, d): the tooltip of the arrow from d to s
  //   onTile(s, parts), onEdge(g, s, d), onBackEdge(g, s), onInput(g): hooks that add behaviour
  //   onStepGroup(g, group, index, label): the same for the frame of a step group and its label
  function drawGraph(svg, w, opts = {}) {
    const { subtitle = WorkflowGraph.subtitle, missing = () => "", selected = null, stateOf = null, input = false, inputEdge = () => true } = opts;
    svg.replaceChildren();
    const defs = el("defs", {}, svg);
    const prefix = opts.idPrefix || "";
    const marker = el("marker", { id: `${prefix}arrow`, viewBox: "0 0 10 10", refX: 2, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse" }, defs);
    el("path", { d: "M0,0 L10,5 L0,10 z", fill: "var(--edge)" }, marker);
    const rejectMarker = el("marker", { id: `${prefix}arrow-reject`, viewBox: "0 0 10 10", refX: 2, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse" }, defs);
    el("path", { d: "M0,0 L10,5 L0,10 z", fill: "var(--failed)" }, rejectMarker);
    if (!w || !w.steps.length) return { root: null, defs };
    const view = opts.view || fitView(w, svg.parentElement?.clientWidth || 600, { input });
    const root = el("g", { class: "graph", transform: `translate(${view.ox},${view.oy}) scale(${view.scale})` }, svg);

    // Step groups first, behind everything: a dashed frame around their steps with the label
    // at its top left. The largest frames are drawn first, so a frame inside another is on top.
    for (const b of stepGroupBoxes(w).sort((x, y) => y.w * y.h - x.w * x.h)) {
      const g = el("g", { class: "step-group", "data-step-group": b.index }, root);
      el("rect", { class: "step-group-frame", x: b.x, y: b.y, width: b.w, height: b.h, rx: 12 }, g);
      const shown = fit(b.label, b.w - 24, 7);
      const text = el("text", { class: "step-group-label", x: b.x + 12, y: b.y + 17 }, g);
      text.textContent = shown;
      if (shown !== b.label) el("title", {}, text).textContent = b.label;
      opts.onStepGroup?.(g, w.groups[b.index], b.index, text);
    }

    for (const s of w.steps) {
      for (const d of s.depends_on || []) {
        const from = stepIn(w, d);
        if (!from) continue;
        const g = el("g", {}, root);
        if (opts.edgeTitle) el("title", {}, g).textContent = opts.edgeTitle(s, d);
        const path = dependencyPath(w, from, s);
        el("path", { d: path, class: "edge-hit" }, g);
        el("path", { d: path, class: "edge", "marker-end": `url(#${prefix}arrow)` }, g);
        opts.onEdge?.(g, s, d);
      }
    }

    // A gate's back edge: where its rejections go. Dashed, in the failed colour, around the tiles.
    for (const s of w.steps) {
      const route = backEdge(w, s);
      if (!route) continue;
      const g = el("g", { class: "back-edge-group", "data-gate": s.id }, root);
      el("title", {}, g).textContent = L().backEdgeTip(s, stepIn(w, s.on_reject), titleCase);
      el("path", { d: route.d, class: "edge-hit" }, g);
      el("path", { d: route.d, class: "back-edge", "marker-end": `url(#${prefix}arrow-reject)` }, g);
      opts.onBackEdge?.(g, s);
    }

    const ip = input ? inputPos(w) : null;
    if (ip) {
      for (const s of w.steps.filter((s) => !(s.depends_on || []).length && inputEdge(s))) {
        el("path", { d: edgePath({ x: ip.x + TILE_W / 2, y: ip.y + TILE_H }, inPort(s)), class: "edge input-edge", "marker-end": `url(#${prefix}arrow)` }, root);
      }
      const g = el("g", { class: "tile input-tile", transform: `translate(${ip.x},${ip.y})`, tabindex: 0, role: "button", "aria-label": "Input" }, root);
      el("title", {}, g).textContent = "Opens Select Inputs to start runs";
      el("rect", { class: "body", width: TILE_W, height: TILE_H, rx: 8 }, g);
      const ic = el("g", { class: "type-icon", transform: "translate(12,24)", stroke: "currentColor", fill: "none", "stroke-width": 1.5, "stroke-linecap": "round", "stroke-linejoin": "round" }, g);
      ic.innerHTML = ICONS.run;
      el("text", { x: 34, y: 37, class: "name" }, g).textContent = "Input";
      el("circle", { class: "port", cx: TILE_W / 2, cy: TILE_H, r: 5 }, g);
      opts.onInput?.(g);
    }

    w.steps.forEach((s, i) => drawTile(root, defs, s, i, { subtitle, missing, selected, stateOf, prefix, onTile: opts.onTile }));
    return { root, defs };
  }

  function drawTile(root, defs, s, i, { subtitle, missing, selected, stateOf, prefix, onTile }) {
    const W = TILE_W, H = TILE_H;
    const gone = missing(s);
    const state = stateOf ? stateOf(s.id) || "idle" : "";
    const g = el("g", {
      class: `tile kind-${kind(s)}${selected === s.id ? " selected" : ""}${gone ? " missing" : ""}${state ? ` state-${state}` : ""}`,
      transform: `translate(${s.position.x},${s.position.y})`,
      tabindex: 0, role: "button", "data-step": s.id,
      "aria-label": `Step ${titleCase(s.id)}:${subtitle(s) ? " " + subtitle(s) : ""}${state ? `, ${STATE_WORD[state] || state}` : ""}`,
    }, root);
    // Every kind of step has its own outline, all inside the same box so ports and arrows stay put.
    // The keyboard's focus ring: 4 px outside the tile, apart from its selection outline.
    el("rect", { class: "focus-ring", x: -4, y: s.service === "team" ? -12 : -4, width: W + (s.service === "team" ? 16 : 8), height: H + (s.service === "team" ? 16 : 8), rx: 11, "aria-hidden": "true" }, g);
    const outline = L().tileOutline(kind(s), W, H);
    const clip = el("clipPath", { id: `${prefix}tile-clip-${i}` }, defs);
    el("path", { d: outline }, clip);
    // A team is a stack of tiles: two more outlines show behind the front one.
    if (s.service === "team") for (const d of [8, 4]) el("rect", { class: "stack", x: d, y: -d, width: W, height: H, rx: 8 }, g);
    const body = el("path", { class: "body", d: outline }, g);
    // A workflow node has a band down each side, the flowchart sign for a process defined elsewhere:
    // it holds a whole workflow. Its icon and text move in by the band, as they do by a slanted edge.
    const pad = TILE_PAD[s.service] || 0;
    if (s.service === "workflow") {
      for (const x of [1, W - pad]) el("rect", { class: "band", x, y: 1, width: pad - 1, height: H - 2, "clip-path": `url(#${prefix}tile-clip-${i})` }, g);
      for (const x of [pad, W - pad]) el("line", { class: "band-line", x1: x, y1: 1, x2: x, y2: H - 1 }, g);
    }
    // A batch is a tray: a second line runs under its top edge.
    if (s.service === "batch") el("line", { class: "tray-line", x1: 1, y1: TRAY_LINE_Y, x2: W - 1, y2: TRAY_LINE_Y }, g);
    const ic = el("g", { class: "type-icon", transform: `translate(${12 + pad},13)`, stroke: "currentColor", fill: "none", "stroke-width": 1.5, "stroke-linecap": "round", "stroke-linejoin": "round" }, g);
    ic.innerHTML = KIND_ICON[s.service] || ICONS[TYPE_ICON[s.service] || "step"];
    const sub = subtitle(s);
    // In a run, the second line is the step's state: a dot and a word, never colour alone.
    const second = state ? STATE_WORD[state] || state : sub;
    if (!second) ic.setAttribute("transform", `translate(${12 + pad},24)`); // a one-line tile centres its name
    const shownName = fit(titleCase(s.id), W - 46 - 2 * pad, 8), shownSub = second ? fit(second, W - 24 - 2 * pad - (state ? 12 : 0)) : "";
    // A tooltip only when the tile cuts its name or second line off, or hides its subtitle for its state.
    if (gone) el("title", {}, g).textContent = `${titleCase(s.id)}: ${gone} no longer exists`; // the reason, always
    else if (shownName !== titleCase(s.id) || shownSub !== second || (state && sub)) el("title", {}, g).textContent = sub ? `${titleCase(s.id)}: ${sub}` : titleCase(s.id);
    el("text", { x: 34 + pad, y: second ? 26 : 37, class: "name" }, g).textContent = shownName;
    if (state) {
      el("circle", { class: `state-dot${state === "blocked" ? " hollow" : ""}`, cx: 16 + pad, cy: 46, r: 4, "aria-hidden": "true" }, g);
      el("text", { x: 26 + pad, y: 50, class: "sub state-word" }, g).textContent = shownSub;
    } else if (sub) el("text", { x: 12 + pad, y: 50, class: "sub" }, g).textContent = shownSub;
    const inPortEl = el("circle", { class: "port", cx: W / 2, cy: 0, r: 5 }, g);
    // Nothing may depend on a route step, so it has no output port.
    const out = s.service !== "route" ? el("circle", { class: "port", cx: W / 2, cy: H, r: 6 }, g) : null;
    // A gate has a second output port, Rejected, on its right edge below the corner: clear of
    // the output port at the bottom and of the arrows that enter a tile's side at its middle.
    const rejected = s.service === "gate" ? el("circle", { class: "port port-reject", cx: W, cy: L().REJECT_PORT_Y, r: 5 }, g) : null;
    onTile?.(s, { g, body, inPort: inPortEl, out, rejected });
  }

  // ---- The read-only viewer: one component for the Runs detail and the Workflows page ----

  // A step's description for its tooltip in the enlarged graph: its name and what it runs or
  // asks, then the description of its agent version (read once per version).
  const agentDescriptions = new Map(); // "name@vN" -> description, "" when it has none or is gone
  async function loadDescriptions(def) {
    const refs = [...new Set((def?.steps || []).map((s) => s.agent).filter((ref) => ref && !agentDescriptions.has(ref)))];
    await Promise.all(refs.map(async (ref) => {
      const at = ref.lastIndexOf("@");
      let text = "";
      try {
        if (at > 0) {
          const stored = await api("GET", `/api/agents/agents/${encodeURIComponent(ref.slice(0, at))}/${encodeURIComponent(ref.slice(at + 1))}`);
          let config = stored?.files?.["agent.json"];
          if (typeof config === "string") config = JSON.parse(config);
          text = typeof config?.description === "string" ? config.description : "";
        }
      } catch { /* an agent that is gone has no description to show */ }
      agentDescriptions.set(ref, text);
    }));
  }
  function describeStep(s) {
    const sub = subtitle(s);
    const text = (typeof s.description === "string" && s.description) || agentDescriptions.get(s.agent) || "";
    return [`${titleCase(s.id)}${sub ? `: ${sub}` : ""}`, text].filter(Boolean).join("\n");
  }

  // viewer draws def read-only in a box that pans (a drag) and zooms (the wheel, around the
  // pointer). It is fitted at first; with a viewKey the panned or zoomed view is kept across
  // redraws. It returns the box, or null for a definition without steps.
  //   width, height: the box (height is the most a fitted graph takes; large fills it)
  //   stateOf(id): the steps' states in a run; the steps the run is at now (running, or
  //     waiting for a person) are marked "active" and drawn in red
  //   selected, onTile(id): the selected step, and what a click or Enter on a tile does
  //   onEnlarge(): a click on the empty graph, and an Enlarge button at its top right
  //   describe(s): each tile's tooltip; graph: further options of drawGraph (subtitle, missing)
  const views = new Map(); // viewKey -> { ox, oy, scale }
  let panning = false; // a pan in progress: the pages do not redraw the graph under the pointer
  const ACTIVE_STATES = ["running", "waiting"];
  const VIEW_MIN_SCALE = 0.15, VIEW_MAX_SCALE = 2.5;
  function viewer(def, opts = {}) {
    if (!def?.steps?.length) return null;
    const { viewKey = "", large = false, stateOf = null, selected = null, onTile = () => {}, onEnlarge = null, onOpen = null, describe = null } = opts;
    const w = { ...def, steps: def.steps.map((s) => ({ ...s, position: s.position ? { ...s.position } : undefined })) };
    autoLayout(w);
    const width = opts.width || 600, maxHeight = opts.height || 360;
    const fitting = { input: false, minScale: large ? VIEW_MIN_SCALE : 0.35, maxScale: large ? 1 : 0.8, margin: 16 };
    let view = fitView(w, width, fitting);
    if (view.height > maxHeight) view = fitView(w, width, { ...fitting, height: maxHeight });
    const height = large ? maxHeight : Math.min(maxHeight, Math.ceil(view.height));
    const kept = viewKey ? views.get(viewKey) : null;
    if (kept) view = { ...view, ...kept };
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", `run-graph run-detail-graph${large ? " large" : ""}`);
    svg.setAttribute("role", "group");
    svg.setAttribute("aria-label", opts.label || "Workflow graph");
    svg.setAttribute("width", width);
    svg.setAttribute("height", height);
    let moved = false; // the pointer went far enough to pan: the click that ends it does nothing
    const wrap = h("div", { className: "run-graph-wrap pannable" }, svg,
      onEnlarge ? iconButton("expand", "Enlarge graph", () => onEnlarge(), { className: "icon-button graph-enlarge" }) : null);
    const drawn = drawGraph(svg, w, {
      ...(opts.graph || {}), view, stateOf, selected, idPrefix: opts.idPrefix || "view-",
      onTile: (s, { g }) => {
        if (stateOf && ACTIVE_STATES.includes(stateOf(s.id))) g.classList.add("active");
        const tip = describe?.(s);
        if (tip) {
          g.querySelector("title")?.remove();
          const title = document.createElementNS(SVG_NS, "title");
          title.textContent = tip;
          g.prepend(title);
        }
        g.addEventListener("click", () => onTile(s.id));
        // A double-click opens the step where it is defined (the Runs detail: on the Workflows page).
        if (onOpen) g.addEventListener("dblclick", (ev) => { ev.stopPropagation(); onOpen(s.id); });
        g.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); onTile(s.id); } });
      },
    });
    const root = drawn.root;
    const apply = () => root?.setAttribute("transform", `translate(${view.ox},${view.oy}) scale(${view.scale})`);
    const keep = () => { if (viewKey) views.set(viewKey, { ox: view.ox, oy: view.oy, scale: view.scale }); };
    let drag = null;
    svg.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0) return;
      drag = { x: ev.clientX, y: ev.clientY, ox: view.ox, oy: view.oy, pointer: ev.pointerId };
      moved = false;
    });
    svg.addEventListener("pointermove", (ev) => {
      if (!drag) return;
      const dx = ev.clientX - drag.x, dy = ev.clientY - drag.y;
      if (!moved && Math.hypot(dx, dy) < 4) return; // a click is not a pan
      if (!moved) { moved = true; panning = true; svg.classList.add("panning"); svg.setPointerCapture?.(drag.pointer); }
      view.ox = drag.ox + dx;
      view.oy = drag.oy + dy;
      apply();
    });
    const endDrag = () => {
      if (!drag) return;
      drag = null;
      if (moved) { keep(); svg.classList.remove("panning"); panning = false; }
    };
    svg.addEventListener("pointerup", endDrag);
    svg.addEventListener("pointercancel", endDrag);
    // Before any tile hears it: the click that ends a pan does nothing, and a click on the
    // empty graph enlarges it.
    svg.addEventListener("click", (ev) => {
      if (moved) { moved = false; ev.stopPropagation(); return; }
      if (onEnlarge && !ev.target.closest?.(".tile")) onEnlarge();
    }, true);
    svg.addEventListener("wheel", (ev) => {
      ev.preventDefault();
      const r = svg.getBoundingClientRect();
      const cx = ev.clientX - r.left, cy = ev.clientY - r.top;
      const next = Math.min(VIEW_MAX_SCALE, Math.max(VIEW_MIN_SCALE, view.scale * Math.exp(-ev.deltaY * 0.0015)));
      const k = next / view.scale;
      view.ox = cx - (cx - view.ox) * k;
      view.oy = cy - (cy - view.oy) * k;
      view.scale = next;
      apply();
      keep();
    }, { passive: false });
    return wrap;
  }

  // openLarge opens a graph nearly full screen in a dialog, to pan and zoom and to read what
  // each step is (its tooltip names it and gives its agent's description). onTile(id, close)
  // answers a click or Enter on a tile. It returns { key, redraw(def, stateOf), close }; a
  // page that shows live states calls redraw, which draws again only when something changed.
  let largeView = null;
  function openLarge({ title, def, stateOf = null, onTile = () => {}, viewKey = "", label = "", graph = {}, onOpen = null, openLabel = "", openTip = "" }) {
    if (largeView || !def?.steps?.length) return largeView;
    const holder = h("div", { className: "run-graph-large" });
    const dlg = openDialog({
      title, body: holder, width: 4000,
      // onOpen adds a button beside Close that leaves for where the graph is defined.
      footer: [onOpen ? h("button", { type: "button", title: openTip, onclick: () => onOpen() }, openLabel) : null, h("button", { type: "button", onclick: () => dlg.close() }, "Close")],
      onClose: () => { largeView = null; },
    });
    dlg.box.classList.add("graph-dialog");
    dlg.box.parentElement.classList.add("graph-backdrop");
    let shown = "";
    const redraw = (d, states = null) => {
      const key = JSON.stringify([d.steps.map((s) => [s.id, states?.(s.id), describeStep(s)]), d.groups, holder.clientWidth, holder.clientHeight]);
      if (key === shown || panning) return; // a redraw would drop the tooltip under the pointer
      shown = key;
      holder.replaceChildren(viewer(d, {
        viewKey: viewKey ? `${viewKey}|large` : "", large: true, stateOf: states, describe: describeStep, label, graph, idPrefix: "large-",
        onTile: (id) => onTile(id, () => dlg.close()),
        width: Math.max(320, holder.clientWidth), height: Math.max(240, holder.clientHeight),
      }));
    };
    largeView = { key: viewKey, redraw, close: () => dlg.close() };
    requestAnimationFrame(() => redraw(def, stateOf)); // once the dialog has its size
    loadDescriptions(def).then(() => { if (largeView?.key === viewKey) redraw(def, stateOf); });
    return largeView;
  }

  return {
    viewer, openLarge, describeStep, loadDescriptions, dragging: () => panning, large: () => largeView,
    SVG_NS, TILE_W, TILE_H, BAND_W, TOP, ROW_H, COL_W, GRAPH_MARGIN, KIND_ICON, TYPE_ICON, STATE_WORD,
    el, kind, subtitle, autoLayout, freeSlot, inPort, outPort, fit, edgePath, laneX, bowsLeft, dependencyPath, backEdge, inputPos, stepGroupBoxes, bounds, fitView, drawGraph,
  };
})();
