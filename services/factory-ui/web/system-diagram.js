"use strict";
// System Diagram: the factory's components and what connects them, drawn from the editable
// source system-diagram.mmd (the flowchart subset below). The page lays the groups out in
// layers that follow the flow, and the drawing can be zoomed (wheel, buttons), moved (a drag
// on the empty canvas), and rearranged (a drag on a component, or on a group to move it with
// all it holds). Every component, group and connection says what it is under the pointer.

// SystemDiagramLogic reads the source and computes the drawing; it touches no page.
const SystemDiagramLogic = (() => {
  const NODE_H = 40, GAP = 12, PAD = 16, TITLE = 28, BETWEEN_LAYERS = 104, BETWEEN_GROUPS = 40;

  // parse reads: `subgraph id[Title]` … `end`, nodes `id[Label]` and `id[(Label)]` (a store),
  // edges `a --> b`, `a <--> b`, `a -. label .-> b` (dashed), and what a component or a group is
  // from a comment `%% describe id: text`. A node an edge names without declaring it is a plain
  // node labelled by its id.
  function parse(source) {
    const groups = [], nodes = [], edges = [], byId = new Map(), described = new Map();
    let group = "";
    const node = (id, label, shape) => {
      if (byId.has(id)) return byId.get(id);
      const made = { id, label: label ?? id, shape: shape || "box", group, description: "" };
      byId.set(id, made); nodes.push(made);
      return made;
    };
    for (const raw of String(source || "").split(/\r?\n/)) {
      const line = raw.trim();
      let m;
      if ((m = /^%%\s*describe\s+([\w-]+)\s*:\s*(.+)$/.exec(line))) { described.set(m[1], m[2].trim()); continue; }
      if (!line || line.startsWith("%%") || /^(flowchart|graph)\b/.test(line)) continue;
      if ((m = /^subgraph\s+([\w-]+)\s*(?:\[(.+)\])?$/.exec(line))) { group = m[1]; groups.push({ id: m[1], title: m[2] || m[1], description: "" }); continue; }
      if (line === "end") { group = ""; continue; }
      if ((m = /^([\w-]+)\s*(<-->|-->|-\.\s*(.*?)\s*\.->)\s*([\w-]+)$/.exec(line))) {
        const inGroup = group; group = "";
        node(m[1]); node(m[4]);
        group = inGroup;
        edges.push({ from: m[1], to: m[4], both: m[2] === "<-->", dashed: m[2].startsWith("-."), label: m[3] || "" });
        continue;
      }
      if ((m = /^([\w-]+)\s*\[\((.+)\)\]$/.exec(line))) { node(m[1], m[2], "store"); continue; }
      if ((m = /^([\w-]+)\s*\[(.+)\]$/.exec(line))) node(m[1], m[2], "box");
    }
    for (const item of [...nodes, ...groups]) item.description = described.get(item.id) || "";
    return { groups: groups.filter((g) => nodes.some((n) => n.group === g.id)), nodes, edges };
  }

  const nodeWidth = (n) => Math.max(120, Math.round(n.label.length * 7.4 + 36));

  // layout places every node, as a layered drawing. A group stands in the layer that the flow
  // gives it: one further than the last group that calls into it (by the solid connections; the
  // dashed ones, such as traces, do not move a group). Inside a layer the groups are ordered to
  // sit beside what they are connected to, which keeps the connections from crossing. The
  // layers run from left to right in a wide panel and from top to bottom in a tall one,
  // whichever shows the drawing larger (aspect = the panel's width / height).
  function layout(model, aspect = 1.6) {
    const units = [];
    for (const g of model.groups) units.push({ id: g.id, boxed: true, nodes: model.nodes.filter((n) => n.group === g.id) });
    for (const n of model.nodes.filter((x) => !x.group)) units.push({ id: `node:${n.id}`, boxed: false, nodes: [n] });
    const unitOf = new Map(units.flatMap((u) => u.nodes.map((n) => [n.id, u])));
    units.forEach((u, index) => {
      u.index = index;
      u.nodeW = Math.max(...u.nodes.map(nodeWidth));
      u.w = u.nodeW + (u.boxed ? 2 * PAD : 0);
      u.h = u.nodes.length * NODE_H + (u.nodes.length - 1) * GAP + (u.boxed ? TITLE + 2 * PAD : 0);
      u.rank = 0;
    });
    const calls = model.edges.filter((e) => !e.dashed).map((e) => [unitOf.get(e.from), unitOf.get(e.to)]).filter(([a, b]) => a && b && a !== b);
    for (let pass = 0; pass < units.length; pass++) for (const [a, b] of calls) if (b.rank < a.rank + 1 && a.rank + 1 < units.length) b.rank = a.rank + 1;
    const ranks = [...new Set(units.map((u) => u.rank))].sort((a, b) => a - b);
    const layers = ranks.map((rank) => units.filter((u) => u.rank === rank));
    const ties = model.edges.map((e) => [unitOf.get(e.from), unitOf.get(e.to), e.dashed ? .35 : 1]).filter(([a, b]) => a && b && a !== b);

    // place gives every group its corner: the layers one after the other along the flow, each
    // layer's groups stacked across it and centred on the tallest layer.
    const place = (across) => {
      const along = across === "y" ? "x" : "y", size = (u, axis) => (axis === "x" ? u.w : u.h);
      const thickness = layers.map((layer) => Math.max(...layer.map((u) => size(u, along === "x" ? "x" : "y"))));
      const extent = layers.map((layer) => layer.reduce((sum, u) => sum + size(u, across), 0) + (layer.length - 1) * BETWEEN_GROUPS);
      const widest = Math.max(...extent);
      let start = 0;
      layers.forEach((layer, i) => {
        let offset = (widest - extent[i]) / 2;
        for (const u of layer) {
          u[along] = start + (thickness[i] - size(u, along)) / 2;
          u[across] = offset;
          offset += size(u, across) + BETWEEN_GROUPS;
        }
        start += thickness[i] + BETWEEN_LAYERS;
      });
      return across === "y" ? { width: start - BETWEEN_LAYERS, height: widest } : { width: widest, height: start - BETWEEN_LAYERS };
    };
    // order sorts the groups of each layer by where their connected groups stand, forwards
    // through the layers and back, a few times: the usual way to take crossings out of layers.
    const order = (across) => {
      const centre = (u) => u[across] + (across === "x" ? u.w : u.h) / 2;
      for (let sweep = 0; sweep < 8; sweep++) {
        place(across);
        for (const layer of sweep % 2 ? [...layers].reverse() : layers) {
          const wish = new Map(layer.map((u) => {
            let sum = 0, weight = 0;
            for (const [a, b, w] of ties) { const other = a === u ? b : b === u ? a : null; if (other && other.rank !== u.rank) { sum += w * centre(other); weight += w; } }
            return [u, weight ? sum / weight : centre(u)];
          }));
          layer.sort((a, b) => wish.get(a) - wish.get(b) || a.index - b.index);
          place(across);
        }
      }
      return place(across);
    };
    // Left to right or top to bottom: whichever fits the panel at the larger scale.
    const fits = (box) => Math.min(aspect / box.width, 1 / box.height);
    const wide = order("y"), wideScale = fits(wide);
    const wideOrder = layers.map((layer) => [...layer]);
    const tall = order("x");
    let across = "x";
    if (wideScale >= fits(tall)) { across = "y"; layers.forEach((layer, i) => layer.splice(0, layer.length, ...wideOrder[i])); place("y"); }

    const positions = {};
    const slot = (u, k) => ({ x: Math.round(u.x + (u.boxed ? PAD : 0) + (u.w - (u.boxed ? 2 * PAD : 0) - u.nodeW) / 2), y: Math.round(u.y + (u.boxed ? TITLE + PAD : 0) + k * (NODE_H + GAP)), w: u.nodeW, h: NODE_H });
    for (const u of units) u.nodes.forEach((n, k) => { positions[n.id] = slot(u, k); });
    // Inside a group, the components take the places that keep their connections short, so a
    // line to a neighbouring group starts beside it instead of running through the group.
    const mid = (p) => ({ x: p.x + p.w / 2, y: p.y + p.h / 2 });
    const reach = (ids) => model.edges.reduce((sum, e) => {
      if (!ids.has(e.from) && !ids.has(e.to)) return sum;
      const a = mid(positions[e.from]), b = mid(positions[e.to]);
      return sum + (e.dashed ? .35 : 1) * Math.hypot(a.x - b.x, a.y - b.y);
    }, 0);
    const orders = (items) => items.length < 2 ? [items] : items.flatMap((item, i) => orders([...items.slice(0, i), ...items.slice(i + 1)]).map((rest) => [item, ...rest]));
    for (let round = 0; round < 2; round++) for (const u of units) {
      if (u.nodes.length < 2 || u.nodes.length > 6) continue;
      const ids = new Set(u.nodes.map((n) => n.id));
      let shortest = reach(ids), kept = u.nodes;
      for (const tried of orders(u.nodes)) {
        tried.forEach((n, k) => { positions[n.id] = slot(u, k); });
        const length = reach(ids);
        if (length < shortest - .5) { shortest = length; kept = tried; }
      }
      u.nodes = kept;
      kept.forEach((n, k) => { positions[n.id] = slot(u, k); });
    }
    return positions;
  }

  // groupRect is the box around a group's nodes, wherever they were dragged, with room for its title.
  function groupRect(model, positions, id) {
    const rects = model.nodes.filter((n) => n.group === id).map((n) => positions[n.id]).filter(Boolean);
    if (!rects.length) return null;
    const x = Math.min(...rects.map((r) => r.x)), y = Math.min(...rects.map((r) => r.y));
    const right = Math.max(...rects.map((r) => r.x + r.w)), bottom = Math.max(...rects.map((r) => r.y + r.h));
    return { x: x - PAD, y: y - PAD - TITLE, w: right - x + 2 * PAD, h: bottom - y + 2 * PAD + TITLE };
  }

  // bounds is the box around the whole drawing.
  function bounds(model, positions) {
    const rects = [...Object.values(positions), ...model.groups.map((g) => groupRect(model, positions, g.id)).filter(Boolean)];
    if (!rects.length) return { x: 0, y: 0, w: 1, h: 1 };
    const x = Math.min(...rects.map((r) => r.x)), y = Math.min(...rects.map((r) => r.y));
    return { x, y, w: Math.max(...rects.map((r) => r.x + r.w)) - x, h: Math.max(...rects.map((r) => r.y + r.h)) - y };
  }

  // edgeRoute is the connection between two nodes as it is drawn, the way a flow chart draws
  // it: it leaves a component at the side that faces the other one and arrives at the facing
  // side, as one smooth curve. Where another component (an obstacle) is in the way, it runs
  // along a free lane between the rows instead, with rounded corners; and two components in
  // one column that are not neighbours are joined by a small bracket beside the column. It
  // returns the path, both ends with the direction the line arrives there (for the
  // arrowheads), its middle (for a label), and how many obstacles it still crosses.
  function edgeRoute(a, b, obstacles = [], index = 0) {
    const flip = (r) => ({ x: r.y, y: r.x, w: r.h, h: r.w });
    const inside = (p, r) => p.x > r.x - 5 && p.x < r.x + r.w + 5 && p.y > r.y - 5 && p.y < r.y + r.h + 5;
    const cubic = (p0, p1, p2, p3, t) => { const u = 1 - t; return { x: u * u * u * p0.x + 3 * u * u * t * p1.x + 3 * u * t * t * p2.x + t * t * t * p3.x, y: u * u * u * p0.y + 3 * u * u * t * p1.y + 3 * u * t * t * p2.y + t * t * t * p3.y }; };
    // A plan is a curve (four points of one cubic) or a line through corners; hits counts the
    // obstacles its samples fall in.
    const measure = (plan, blocks) => {
      const samples = [];
      if (plan.curve) for (let step = 1; step < 24; step++) samples.push(cubic(...plan.curve, step / 24));
      else for (let i = 1; i < plan.corners.length; i++) {
        const p = plan.corners[i - 1], q = plan.corners[i], steps = Math.max(1, Math.round(Math.hypot(q.x - p.x, q.y - p.y) / 10));
        for (let step = 0; step <= steps; step++) samples.push({ x: p.x + (q.x - p.x) * step / steps, y: p.y + (q.y - p.y) * step / steps });
      }
      plan.hits = new Set(samples.flatMap((p) => blocks.map((r, i) => (inside(p, r) ? i : -1)).filter((i) => i >= 0))).size;
      plan.length = samples.reduce((sum, p, i) => sum + (i ? Math.hypot(p.x - samples[i - 1].x, p.y - samples[i - 1].y) : 0), 0);
      return plan;
    };
    // across joins two components that stand side by side: out of the side of a that faces b,
    // into the side of b that faces a.
    const across = (s0, t0, blocks) => {
      const dir = t0.x + t0.w / 2 >= s0.x + s0.w / 2 ? 1 : -1;
      const s = { x: dir > 0 ? s0.x + s0.w : s0.x, y: s0.y + s0.h / 2 }, t = { x: dir > 0 ? t0.x : t0.x + t0.w, y: t0.y + t0.h / 2 };
      const pull = Math.max(28, Math.abs(t.x - s.x) * .45);
      const direct = measure({ curve: [s, { x: s.x + dir * pull, y: s.y }, { x: t.x - dir * pull, y: t.y }, t], mid: null, end: dir > 0 ? 0 : Math.PI }, blocks);
      direct.mid = cubic(...direct.curve, .5);
      if (!direct.hits) return direct;
      // The lanes: just above and just below every component between the two, and the two ends' own rows.
      const out = s.x + dir * 26, back = t.x - dir * 26, low = Math.min(out, back), high = Math.max(out, back), nudge = ((index * 7) % 5 - 2) * 4;
      const lanes = [s.y, t.y, ...blocks.filter((r) => r.x + r.w > low - 8 && r.x < high + 8).flatMap((r) => [r.y - 16 + nudge, r.y + r.h + 16 + nudge])];
      let best = direct;
      for (const y of lanes) {
        const plan = measure({ corners: [s, { x: out, y: s.y }, { x: out, y }, { x: back, y }, { x: back, y: t.y }, t], mid: { x: (out + back) / 2, y }, end: dir > 0 ? 0 : Math.PI }, blocks);
        if (plan.hits < best.hits || (plan.hits === best.hits && !best.curve && plan.length < best.length)) best = plan;
      }
      return best;
    };
    const hGap = Math.max(b.x - (a.x + a.w), a.x - (b.x + b.w)), vGap = Math.max(b.y - (a.y + a.h), a.y - (b.y + b.h));
    // One above the other: the same as side by side, turned a quarter (out of the bottom, into the top).
    const upright = () => {
      const turned = across(flip(a), flip(b), obstacles.map(flip)), unturn = (p) => ({ x: p.y, y: p.x });
      return { hits: turned.hits, length: turned.length, mid: unturn(turned.mid), end: Math.PI / 2 - turned.end, curve: turned.curve?.map(unturn), corners: turned.corners?.map(unturn) };
    };
    let plan;
    if (hGap > 24 && vGap > 24) {
      // Apart both ways: the wider gap decides the sides, unless the other way crosses less.
      const [first, second] = hGap >= vGap ? [across(a, b, obstacles), upright()] : [upright(), across(a, b, obstacles)];
      plan = second.hits < first.hits ? second : first;
    } else if (hGap >= vGap && hGap > 24) plan = across(a, b, obstacles);
    else if (vGap > 0) {
      plan = upright();
      const sameColumn = Math.abs(a.x + a.w / 2 - (b.x + b.w / 2)) < Math.min(a.w, b.w) / 2;
      if (plan.hits && sameColumn) {
        const reach = Math.max(28, Math.min(72, 22 + Math.abs(b.y - a.y) * .14));
        for (const side of [1, -1]) {
          const s = { x: side > 0 ? a.x + a.w : a.x, y: a.y + a.h / 2 }, t = { x: side > 0 ? b.x + b.w : b.x, y: b.y + b.h / 2 };
          const beside = measure({ curve: [s, { x: s.x + side * reach, y: s.y }, { x: t.x + side * reach, y: t.y }, t], end: side > 0 ? Math.PI : 0, start: side > 0 ? Math.PI : 0 }, obstacles);
          beside.mid = cubic(...beside.curve, .5);
          if (beside.hits < plan.hits) plan = beside;
        }
      }
    } else {
      // Overlapping: a plain line from centre to centre, cut at the borders.
      const centre = (r) => ({ x: r.x + r.w / 2, y: r.y + r.h / 2 }), ca = centre(a), cb = centre(b);
      const leave = (r, c, toward) => { const dx = toward.x - c.x, dy = toward.y - c.y; if (!dx && !dy) return c; const k = Math.min(dx ? r.w / 2 / Math.abs(dx) : Infinity, dy ? r.h / 2 / Math.abs(dy) : Infinity); return { x: c.x + dx * k, y: c.y + dy * k }; };
      const s = leave(a, ca, cb), t = leave(b, cb, ca);
      plan = measure({ corners: [s, t], mid: { x: (s.x + t.x) / 2, y: (s.y + t.y) / 2 }, end: Math.atan2(t.y - s.y, t.x - s.x) }, obstacles);
    }
    const n = (v) => v.toFixed(1), at = (p) => `${n(p.x)} ${n(p.y)}`;
    let d, first, last;
    if (plan.curve) {
      [first, , , last] = plan.curve;
      const flat = Math.abs(first.y - last.y) < .5 || Math.abs(first.x - last.x) < .5;
      d = flat && plan.start === undefined ? `M${at(first)} L${at(last)}` : `M${at(first)} C${at(plan.curve[1])} ${at(plan.curve[2])} ${at(last)}`;
    } else {
      // Corners are rounded; a corner of no length (the lane is the row of an end) is skipped.
      const corners = plan.corners.filter((p, i, all) => !i || Math.hypot(p.x - all[i - 1].x, p.y - all[i - 1].y) > .5);
      first = corners[0]; last = corners[corners.length - 1];
      d = `M${at(first)}`;
      for (let i = 1; i < corners.length - 1; i++) {
        const p = corners[i], before = corners[i - 1], after = corners[i + 1];
        const lenIn = Math.hypot(p.x - before.x, p.y - before.y), lenOut = Math.hypot(after.x - p.x, after.y - p.y), r = Math.min(12, lenIn / 2, lenOut / 2);
        d += ` L${at({ x: p.x - (p.x - before.x) / lenIn * r, y: p.y - (p.y - before.y) / lenIn * r })} Q${at(p)} ${at({ x: p.x + (after.x - p.x) / lenOut * r, y: p.y + (after.y - p.y) / lenOut * r })}`;
      }
      d += ` L${at(last)}`;
    }
    return {
      d, x1: first.x, y1: first.y, x2: last.x, y2: last.y, mid: plan.mid, crossings: plan.hits, shape: plan.curve ? "curve" : "lane",
      endAngle: plan.end, startAngle: plan.start ?? plan.end + Math.PI,
    };
  }

  // fit is the zoom and offset that show a box whole and centred in a view, with a margin.
  function fit(box, viewW, viewH, margin = 32) {
    const k = Math.max(.2, Math.min(2, (viewW - 2 * margin) / box.w, (viewH - 2 * margin) / box.h));
    return { k, x: (viewW - box.w * k) / 2 - box.x * k, y: (viewH - box.h * k) / 2 - box.y * k };
  }

  // describe is what the card under the pointer says about a component ({ kind: "node", id }),
  // a group ({ kind: "group", id }) or a connection ({ kind: "edge", index }): its name, what it
  // is, and a few lines on what it is connected to. No sentence ends with a period.
  function describe(model, target) {
    const name = (id) => model.nodes.find((n) => n.id === id)?.label || id;
    const list = (label, items) => (items.length ? `${label}: ${[...new Set(items)].join(", ")}` : "");
    if (target.kind === "group") {
      const g = model.groups.find((x) => x.id === target.id);
      if (!g) return null;
      return { title: g.title, text: g.description, lines: [list("Holds", model.nodes.filter((n) => n.group === g.id).map((n) => n.label))].filter(Boolean) };
    }
    if (target.kind === "edge") {
      const e = model.edges[target.index];
      if (!e) return null;
      const a = name(e.from), b = name(e.to);
      const text = e.dashed ? `${a} reaches ${b}${e.label ? `: ${e.label}` : ""}` : e.both ? `${a} and ${b} call each other` : `${a} calls ${b}${e.label ? `: ${e.label}` : ""}`;
      return { title: `${a} ${e.both ? "↔" : "→"} ${b}`, text, lines: e.dashed ? ["Dashed: beside the main flow of calls"] : [] };
    }
    const n = model.nodes.find((x) => x.id === target.id);
    if (!n) return null;
    const out = model.edges.filter((e) => e.from === n.id), inn = model.edges.filter((e) => e.to === n.id);
    const also = (e, id) => `${name(id)}${e.label ? ` (${e.label})` : ""}`;
    const group = model.groups.find((g) => g.id === n.group);
    return {
      title: n.label, text: n.description,
      lines: [
        group ? `In: ${group.title}` : "",
        list("Calls", [...out.filter((e) => !e.dashed).map((e) => name(e.to)), ...inn.filter((e) => !e.dashed && e.both).map((e) => name(e.from))]),
        list("Called by", [...inn.filter((e) => !e.dashed).map((e) => name(e.from)), ...out.filter((e) => !e.dashed && e.both).map((e) => name(e.to))]),
        list("Also reaches", out.filter((e) => e.dashed).map((e) => also(e, e.to))),
        list("Also reached by", inn.filter((e) => e.dashed).map((e) => also(e, e.from))),
      ].filter(Boolean),
    };
  }

  // lit tells which components, groups and connections belong to what is under the pointer:
  // a component with its connections and their other ends, a connection with its two ends, a
  // group with all it holds and every connection of those.
  function lit(model, target) {
    const nodes = new Set(), edges = new Set(), groups = new Set();
    if (!target) return { nodes, edges, groups };
    const touch = (index) => { const e = model.edges[index]; edges.add(index); nodes.add(e.from); nodes.add(e.to); };
    if (target.kind === "edge") { if (model.edges[target.index]) touch(target.index); } else {
      const ids = target.kind === "group" ? model.nodes.filter((n) => n.group === target.id).map((n) => n.id) : [target.id];
      if (target.kind === "group") groups.add(target.id);
      for (const id of ids) nodes.add(id);
      model.edges.forEach((e, index) => { if (ids.includes(e.from) || ids.includes(e.to)) touch(index); });
    }
    return { nodes, edges, groups };
  }

  return { parse, layout, groupRect, bounds, edgeRoute, fit, describe, lit, nodeWidth, NODE_H };
})();

(() => {
  const L = SystemDiagramLogic, body = $("system-diagram"), actions = $("system-diagram-actions");
  const KEY = "factory.systemDiagram.positions", NS = "http://www.w3.org/2000/svg";
  const state = { model: null, positions: {}, view: { k: 1, x: 0, y: 0 }, drag: null, target: null, parts: { nodes: new Map(), groups: new Map(), edges: [] } };
  let loaded = false, request = 0, svg = null, scene = null, tip = null;

  const el = (tag, attrs = {}, ...children) => {
    const node = document.createElementNS(NS, tag);
    for (const [key, value] of Object.entries(attrs)) if (value !== null && value !== undefined) node.setAttribute(key, String(value));
    for (const child of children) if (child !== null && child !== undefined) node.append(child);
    return node;
  };
  const size = () => { const box = body.getBoundingClientRect?.() || {}; return { w: box.width || 960, h: box.height || 600 }; };
  const saved = () => { try { return JSON.parse(localStorage.getItem(KEY) || "{}") || {}; } catch { return {}; } };
  function remember() {
    const moved = Object.fromEntries(Object.entries(state.positions).map(([id, p]) => [id, { x: p.x, y: p.y }]));
    try { localStorage.setItem(KEY, JSON.stringify(moved)); } catch { /* storage unavailable */ }
  }

  // arrange computes the layout for the panel's shape; places a person dragged components to win.
  function arrange(keep = true) {
    const view = size(), placed = L.layout(state.model, view.w / view.h), kept = keep ? saved() : {};
    for (const [id, p] of Object.entries(placed)) if (kept[id] && Number.isFinite(kept[id].x) && Number.isFinite(kept[id].y)) { p.x = kept[id].x; p.y = kept[id].y; }
    state.positions = placed;
  }
  function fitView() { const view = size(); state.view = L.fit(L.bounds(state.model, state.positions), view.w, view.h); applyView(); }
  const applyView = () => scene?.setAttribute("transform", `translate(${state.view.x} ${state.view.y}) scale(${state.view.k})`);
  function zoom(factor, cx, cy) {
    const view = size(), k = Math.max(.2, Math.min(4, state.view.k * factor));
    const x = cx ?? view.w / 2, y = cy ?? view.h / 2, ratio = k / state.view.k;
    state.view = { k, x: x - (x - state.view.x) * ratio, y: y - (y - state.view.y) * ratio };
    applyView();
  }

  function arrow(x, y, angle) {
    const point = (a, d) => `${(x + Math.cos(angle + a) * d).toFixed(1)} ${(y + Math.sin(angle + a) * d).toFixed(1)}`;
    return el("path", { class: "sd-arrow", d: `M${x.toFixed(1)} ${y.toFixed(1)} L${point(Math.PI - .42, 9)} L${point(Math.PI + .42, 9)} Z` });
  }

  // draw rebuilds the drawing: the groups behind, the connections, then the components.
  function draw() {
    const { model, positions } = state, parts = { nodes: new Map(), groups: new Map(), edges: [] };
    const groups = model.groups.map((g) => {
      const r = L.groupRect(model, positions, g.id);
      const part = el("g", { class: "sd-group", "data-group": g.id }, el("rect", { x: r.x, y: r.y, width: r.w, height: r.h, rx: 10 }));
      parts.groups.set(g.id, { part, base: "sd-group" });
      return part;
    });
    // A group's name is drawn over the connections, so a line never strikes it out.
    const names = model.groups.map((g) => {
      const r = L.groupRect(model, positions, g.id);
      return el("text", { class: "sd-group-name", x: r.x + 16, y: r.y + 20 }, g.title);
    });
    const edges = model.edges.map((e, index) => {
      const a = positions[e.from], b = positions[e.to];
      if (!a || !b) return null;
      // Every other component is in the way of this connection: it goes around them.
      const line = L.edgeRoute(a, b, Object.entries(positions).filter(([id]) => id !== e.from && id !== e.to).map(([, r]) => r));
      const base = `sd-edge${e.dashed ? " sd-dashed" : ""}`;
      // The wide, unseen line under the thin one is what the pointer finds.
      const part = el("g", { class: base, "data-edge": index },
        el("path", { class: "sd-hit", d: line.d }), el("path", { class: "sd-line", d: line.d }),
        arrow(line.x2, line.y2, line.endAngle), e.both ? arrow(line.x1, line.y1, line.startAngle) : null,
        e.label ? el("text", { class: "sd-edge-label", x: line.mid.x, y: line.mid.y - 4, "text-anchor": "middle" }, e.label) : null);
      parts.edges[index] = { part, base };
      return part;
    });
    const nodes = model.nodes.map((n) => {
      const p = positions[n.id], base = `sd-node${n.shape === "store" ? " sd-store" : ""}`;
      const part = el("g", { class: base, "data-node": n.id, tabindex: 0, role: "img", "aria-label": cardText(L.describe(model, { kind: "node", id: n.id })) },
        el("rect", { x: p.x, y: p.y, width: p.w, height: p.h, rx: n.shape === "store" ? p.h / 2 : 8 }),
        el("text", { x: p.x + p.w / 2, y: p.y + p.h / 2 + 5, "text-anchor": "middle" }, n.label));
      parts.nodes.set(n.id, { part, base });
      return part;
    });
    state.parts = parts;
    scene.replaceChildren(...groups, ...edges.filter(Boolean), ...names, ...nodes);
    highlight();
  }

  // highlight marks what belongs to the thing under the pointer and dims every other
  // connection. It changes classes only, so the drawing, and the keyboard focus, stay as they are.
  function highlight() {
    const on = L.lit(state.model, state.target), any = !!state.target;
    for (const [id, { part, base }] of state.parts.nodes) part.setAttribute("class", `${base}${on.nodes.has(id) ? " sd-lit" : ""}`);
    for (const [id, { part, base }] of state.parts.groups) part.setAttribute("class", `${base}${on.groups.has(id) ? " sd-lit" : ""}`);
    state.parts.edges.forEach((edge, index) => edge?.part.setAttribute("class", `${edge.base}${on.edges.has(index) ? " sd-lit" : any ? " sd-dim" : ""}`));
  }

  const cardText = (card) => (card ? [card.title, card.text, ...card.lines].filter(Boolean).join(". ") : "");
  // showCard puts the card beside a point of the panel (x, y from its corner), inside the panel.
  function showCard(card, x, y) {
    if (!card) { hideCard(); return; }
    // What something is comes first when the source says it; then what it connects to.
    const about = card.text ? [h("p", { className: "sd-tip-text" }, card.text)] : [];
    tip.replaceChildren(h("strong", { className: "sd-tip-title" }, card.title), ...about, ...card.lines.map((line) => h("p", { className: "sd-tip-line" }, line)));
    tip.hidden = false;
    const view = size(), width = tip.offsetWidth || 300, height = tip.offsetHeight || 0;
    tip.style.left = `${Math.max(8, Math.min(x + 16, view.w - width - 8))}px`;
    tip.style.top = `${y + 20 + height > view.h - 8 ? Math.max(8, y - height - 12) : y + 20}px`;
  }
  function hideCard() { if (tip) tip.hidden = true; }
  const targetOf = (element) => {
    const node = element?.closest?.(".sd-node"), edge = element?.closest?.(".sd-edge"), group = element?.closest?.(".sd-group");
    return node ? { kind: "node", id: node.dataset.node } : edge ? { kind: "edge", index: Number(edge.dataset.edge) } : group ? { kind: "group", id: group.dataset.group } : null;
  };
  const same = (a, b) => (!a && !b) || (a && b && a.kind === b.kind && a.id === b.id && a.index === b.index);
  // point is where the pointer is, from the corner of the panel.
  const point = (ev) => { const box = body.getBoundingClientRect?.() || { left: 0, top: 0 }; return { x: ev.clientX - box.left, y: ev.clientY - box.top }; };
  function hover(target, at) {
    if (!same(target, state.target)) { state.target = target; highlight(); }
    if (target && at) showCard(L.describe(state.model, target), at.x, at.y); else hideCard();
  }

  function build() {
    scene = el("g", { class: "sd-scene" });
    svg = el("svg", { class: "sd-canvas", role: "group", "aria-label": "System diagram: the factory's components and their connections", tabindex: 0 }, scene);
    tip = h("div", { className: "sd-tip", role: "tooltip", hidden: true });
    svg.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0) return;
      const node = ev.target.closest?.(".sd-node"), group = ev.target.closest?.(".sd-group");
      const ids = node ? [node.dataset.node] : group ? state.model.nodes.filter((n) => n.group === group.dataset.group).map((n) => n.id) : null;
      state.drag = { x: ev.clientX, y: ev.clientY, ids, start: ids ? Object.fromEntries(ids.map((id) => [id, { ...state.positions[id] }])) : { ...state.view }, moved: false };
      svg.setPointerCapture?.(ev.pointerId);
      svg.classList.add(ids ? "sd-moving" : "sd-panning");
      ev.preventDefault();
    });
    svg.addEventListener("pointermove", (ev) => {
      const d = state.drag;
      // Nothing held: the card follows the pointer over whatever it is on.
      if (!d) { hover(targetOf(ev.target), point(ev)); return; }
      const dx = ev.clientX - d.x, dy = ev.clientY - d.y;
      if (!d.moved && Math.hypot(dx, dy) < 3) return;
      d.moved = true;
      hideCard();
      if (d.ids) {
        for (const id of d.ids) { state.positions[id].x = Math.round(d.start[id].x + dx / state.view.k); state.positions[id].y = Math.round(d.start[id].y + dy / state.view.k); }
        draw();
      } else { state.view = { k: state.view.k, x: d.start.x + dx, y: d.start.y + dy }; applyView(); }
    });
    const drop = () => {
      if (state.drag?.ids && state.drag.moved) remember();
      state.drag = null;
      svg.classList.remove("sd-moving", "sd-panning");
    };
    svg.addEventListener("pointerup", drop);
    svg.addEventListener("pointercancel", drop);
    svg.addEventListener("pointerleave", () => { if (!state.drag) hover(null); });
    svg.addEventListener("wheel", (ev) => { ev.preventDefault(); hideCard(); const at = point(ev); zoom(Math.exp(-ev.deltaY * .0015), at.x, at.y); }, { passive: false });
    svg.addEventListener("dblclick", (ev) => { if (!ev.target.closest?.(".sd-node, .sd-group")) fitView(); });
    // With the keyboard, Tab walks the components: each shows its card and its connections.
    svg.addEventListener("focusin", (ev) => {
      const target = targetOf(ev.target);
      if (!target) return;
      const box = ev.target.getBoundingClientRect?.(), panel = body.getBoundingClientRect?.();
      hover(target, box && panel ? { x: box.left - panel.left, y: box.bottom - panel.top - 12 } : { x: 0, y: 0 });
    });
    svg.addEventListener("focusout", () => hover(null));
    svg.addEventListener("keydown", (ev) => {
      if (ev.key === "+" || ev.key === "=") zoom(1.2);
      else if (ev.key === "-") zoom(1 / 1.2);
      else if (ev.key === "0") fitView();
      else if (ev.key === "Escape") hideCard();
      else return;
      ev.preventDefault();
    });
    body.replaceChildren(svg, tip);
  }

  async function load() {
    const current = ++request;
    body.replaceChildren(emptyPanel("Loading system diagram…"));
    try {
      const response = await fetch("system-diagram.mmd", { cache: "no-store" });
      if (!response.ok) throw new Error(`the diagram file answered ${response.status}`);
      const source = await response.text();
      if (current !== request) return;
      const model = L.parse(source);
      if (!model.nodes.length) throw new Error("the diagram file names no component");
      state.model = model;
      build(); arrange(); draw(); fitView();
      loaded = true;
    } catch (error) {
      if (current !== request) return;
      body.replaceChildren(errorPanel(`Cannot load system diagram: ${error.message}`));
    }
  }

  const button = (label, name, tip, run) => h("button", { type: "button", "aria-label": name, title: tip, onclick: () => { if (loaded) run(); } }, label);
  actions?.replaceChildren(
    button("−", "Zoom out", "Makes the diagram smaller", () => zoom(1 / 1.25)),
    button("+", "Zoom in", "Makes the diagram larger", () => zoom(1.25)),
    button("Fit", "Fit", "Shows the whole diagram", fitView),
    button("Reset layout", "Reset layout", "Puts every component back where the layout places it", () => {
      try { localStorage.removeItem(KEY); } catch { /* storage unavailable */ }
      arrange(false); draw(); fitView();
    }));

  tabs["system-diagram"] = {
    show() { if (!loaded) load(); }, // a second visit keeps the zoom and place it was left at
    hide() { ++request; hideCard(); },
  };
})();
