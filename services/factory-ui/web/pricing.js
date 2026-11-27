"use strict";
(() => {
  const body = measurementUI.panel("pricing", true), state = { kind: "prices", current: null, active: false };
  const kind = measurementUI.select("pricing-kind", [["prices", "Model prices"], ["conversions", "Currency conversion"]]);
  const currency = measurementUI.select("pricing-display", [["", "Not set"], ["EUR", "EUR"], ["USD", "USD"]]);
  currency.value = localStorage.getItem("factory.displayCurrency") || "";
  currency.onchange = () => localStorage.setItem("factory.displayCurrency", currency.value);
  const list = h("div", { className: "measurement-list price-table" }), editor = h("form", { className: "measurement-editor", hidden: true });
  // The filter keeps the prices whose model or vendor contains the typed text, whatever its case.
  const filter = h("input", { id: "pricing-filter", type: "search", placeholder: "Filter", autocomplete: "off", "aria-label": "Filter prices by model or vendor", oninput: () => renderList() });
  const filterField = measurementUI.field("Model", filter);
  const EFFORTS = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];
  const effortRank = (effort) => { const at = EFFORTS.indexOf(effort); return at < 0 ? EFFORTS.length : at; };
  const day = (iso) => { const at = new Date(iso); return Number.isFinite(+at) ? at.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" }) : String(iso ?? ""); };
  const perTokens = (n) => `${new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 }).format(Number(n) || 0)} tokens`;
  const row = typeof clickableRow === "function" ? clickableRow : (tr, run) => { tr.onclick = run; return tr; };
  const job = h("p", { role: "status", className: "muted" }); let timer = null;
  // Prices and conversions have no ids: each is addressed by its natural key.
  const add = iconButton("plus", "New price", () => edit({}));
  const priceKey = (p) => new URLSearchParams({ vendor: p.vendor, model: p.model, effort: p.effort, modes: JSON.stringify(p.modes), from: p.from }).toString();
  const sameKey = (a, b) => a.vendor === b.vendor && a.model === b.model && a.effort === b.effort && JSON.stringify(a.modes) === JSON.stringify(b.modes) && Date.parse(a.from) === Date.parse(b.from);
  const conversionKey = (c) => new URLSearchParams({ from_currency: c.from_currency, to_currency: c.to_currency }).toString();
  const sameConversionKey = (a, b) => a.from_currency === b.from_currency && a.to_currency === b.to_currency;
  const match = (revision) => ({ "If-Match": revision.startsWith('"') ? revision : `"${revision}"` });
  // savePrice creates a new row with POST and changes an existing one with PUT by its key. A
  // changed key, such as a new start date, deletes the old row and creates the new one.
  async function savePrice(item, value) {
    delete value.revision;
    if (item.revision && sameKey(item, value)) return api("PUT", `/api/evaluations/prices?${priceKey(item)}`, value, match(item.revision));
    if (item.revision) {
      await api("DELETE", `/api/evaluations/prices?${priceKey(item)}`, undefined, match(item.revision));
      delete item.revision;
    }
    return api("POST", "/api/evaluations/prices", value);
  }
  async function saveConversion(item, value) {
    delete value.revision;
    if (item.revision && sameConversionKey(item, value)) return api("PUT", `/api/evaluations/conversions?${conversionKey(item)}`, value, match(item.revision));
    if (item.revision) {
      await api("DELETE", `/api/evaluations/conversions?${conversionKey(item)}`, undefined, match(item.revision));
      delete item.revision;
    }
    return api("POST", "/api/evaluations/conversions", value);
  }
  const recalc = h("button", { type: "button", onclick: recalculate }, "Recalculate costs");
  const backfill = h("button", { type: "button", onclick: importRetained }, "Import retained runs");
  body.append(h("div", { className: "measurement-filters" }, measurementUI.field("Table", kind), filterField, measurementUI.field("Display currency", currency), add, recalc, backfill), job, list, editor);
  kind.onchange = () => { if (!editor.hidden) { kind.value = state.kind; notify("Finish or cancel the current edit", "error"); return; } state.kind = kind.value; add.title = state.kind === "prices" ? "New price" : "New conversion"; filterField.hidden = state.kind !== "prices"; state.items = []; load(); };
  async function load() {
    try {
      const which = state.kind, data = await api("GET", `/api/evaluations/${which}`);
      if (!state.active || which !== state.kind) return;
      state.items = measurementUI.rows(data);
      renderList();
    } catch (error) { list.replaceChildren(errorPanel(`Cannot load prices: ${error.message}`)); }
  }
  // renderList draws the table of the chosen kind: a price per row, sorted by vendor, model and
  // effort, with every rate in its own right-aligned column; a click on a row edits it.
  function renderList() {
    const items = state.items || [], selected = (item) => String(!editor.hidden && state.current === item);
    if (state.kind !== "prices") {
      if (!items.length) { list.replaceChildren(emptyPanel("No conversion rates")); return; }
      list.replaceChildren(dataTable(["From", "To", "Rate"], items.map(item => row(h("tr", { "aria-selected": selected(item) },
        h("td", {}, item.from_currency), h("td", {}, item.to_currency), h("td", { className: "num" }, String(item.rate))), () => edit(item))), { numeric: [2] }));
      return;
    }
    if (!items.length) { list.replaceChildren(emptyPanel("No prices")); return; }
    const text = filter.value.trim().toLowerCase();
    const shown = items.filter(item => !text || `${item.model} ${item.vendor}`.toLowerCase().includes(text))
      .sort((a, b) => String(a.vendor).localeCompare(String(b.vendor)) || String(a.model).localeCompare(String(b.model)) || effortRank(a.effort) - effortRank(b.effort) || String(b.from).localeCompare(String(a.from)));
    if (!shown.length) { list.replaceChildren(h("p", { className: "muted", role: "status" }, `No model or vendor contains “${filter.value.trim()}”`)); return; }
    list.replaceChildren(dataTable(["Model", "Vendor", "Effort", "Input", "Cached input", "Output", "Per", "Currency", "Effective"], shown.map(item => row(h("tr", { "aria-selected": selected(item) },
      h("td", { className: "name-cell", title: item.model, dataset: { full: item.model } }, item.model), h("td", {}, item.vendor), h("td", {}, item.effort),
      h("td", { className: "num" }, String(item.input_uncached)), h("td", { className: "num" }, String(item.input_cached)), h("td", { className: "num" }, String(item.output)),
      h("td", { className: "num" }, perTokens(item.unit_tokens)), h("td", {}, item.currency),
      h("td", { title: item.to ? `${item.from} to ${item.to}` : item.from }, item.to ? `${day(item.from)} to ${day(item.to)}` : `Since ${day(item.from)}`)), () => edit(item))),
    { numeric: [3, 4, 5, 6], name: 0 }));
  }
  function edit(item) {
    state.current = item; editor.hidden = false; const controls = {};
    renderList();
    Promise.resolve().then(() => editor.scrollIntoView?.({ block: "nearest" }));
    const field = (key, label, type = "text") => {
      let input;
      if (type === "currency") input = measurementUI.select(`price-${key}`, [["", "Not set"], ["EUR", "EUR"], ["USD", "USD"]]);
      else input = h("input", { id: `price-${key}`, type: "text", inputMode: type === "decimal" ? "decimal" : undefined });
      input.value = item[key] ?? ""; input.required = true; controls[key] = input; return measurementUI.field(label, input);
    };
    const fields = state.kind === "prices" ? [field("vendor", "Vendor"), field("model", "Model"), field("effort", "Effort"), field("currency", "Currency", "currency"), field("unit_tokens", "Tokens per rate", "decimal"), field("input_uncached", "Uncached input rate", "decimal"), field("input_cached", "Cached input rate", "decimal"), field("output", "Output rate", "decimal"), field("from", "Effective from (UTC)"), field("to", "Effective to (UTC)")] : [field("from_currency", "Source currency", "currency"), field("to_currency", "Display currency", "currency"), field("rate", "Conversion rate", "decimal")];
    let open, modes;
    if (state.kind === "prices") {
      controls.to.required = false;
      open = h("input", { type: "checkbox", id: "price-open", checked: item.to === null });
      open.onchange = () => { controls.to.disabled = open.checked; };
      controls.to.disabled = open.checked;
      modes = h("textarea", { id: "price-modes", rows: 2, required: true, value: item.modes === undefined ? "" : JSON.stringify(item.modes), placeholder: "{}" });
      fields.push(h("label", { className: "measurement-checkbox" }, open, "No end date"), measurementUI.field("Modes (JSON)", modes));
    }
    const problem = h("p", { className: "error-text", role: "alert" }), save = h("button", { type: "submit", className: "primary" }, "Save");
    editor.replaceChildren(...fields, problem, h("div", { className: "measurement-actions" }, h("button", { type: "button", onclick: () => { editor.hidden = true; state.current = null; renderList(); } }, "Cancel"), save));
    editor.onsubmit = async event => {
      event.preventDefault(); const value = { ...item };
      try {
        for (const [key, input] of Object.entries(controls)) value[key] = input.value.trim();
        if (state.kind === "prices") {
          value.modes = JSON.parse(modes.value);
          if (!value.modes || Array.isArray(value.modes) || typeof value.modes !== "object") throw new Error("Modes must be a JSON object.");
          if (!open.checked && !value.to) throw new Error("Enter an end date or select No end date.");
          if (!/(Z|[+-]\d\d:\d\d)$/.test(value.from) || (!open.checked && !/(Z|[+-]\d\d:\d\d)$/.test(value.to))) throw new Error("Effective dates must include Z or an explicit timezone offset.");
          // An unchanged start date keeps its stored text, since it is part of the price's key.
          value.from = value.from === item.from ? item.from : new Date(value.from).toISOString(); value.to = open.checked ? null : new Date(value.to).toISOString();
          value.unit_tokens = Number(value.unit_tokens);
          if (!Number.isSafeInteger(value.unit_tokens) || value.unit_tokens <= 0) throw new Error("Tokens per rate must be a positive integer.");
        }
        save.disabled = true;
        if (state.kind === "prices") await savePrice(item, value);
        else await saveConversion(item, value);
        editor.hidden = true; state.current = null; notify("Saved"); await load();
      } catch (error) { problem.textContent = `Not saved: ${error.message}`; }
      finally { save.disabled = false; }
    };
  }
  async function recalculate() {
    if (!currency.value) { job.textContent = "Select a display currency."; return; }
    recalc.disabled = true;
    try {
      const result = await api("POST", "/api/evaluations/recalculations", { display_currency: currency.value });
      const poll = async () => {
        try {
          const value = await api("GET", `/api/evaluations/recalculations/${encodeURIComponent(result.id)}`);
          if (!state.active) return;
          job.textContent = `${value.status} · ${value.processed ?? "Unknown"} processed${value.error ? ` · ${value.error}` : ""}`;
          if (["complete", "completed", "failed", "start_failed"].includes(value.status)) clearInterval(timer);
        } catch (error) { job.textContent = `Cannot read recalculation: ${error.message}`; }
      };
      clearInterval(timer); await poll(); timer = setInterval(poll, 2500);
    } catch (error) { job.textContent = `Cannot start recalculation: ${error.message}`; }
    finally { recalc.disabled = false; }
  }
  async function importRetained() {
    backfill.disabled = true;
    try {
      const result = await api("POST", "/api/evaluations/backfills", {});
      const poll = async () => {
        try {
          const value = await api("GET", `/api/evaluations/backfills/${encodeURIComponent(result.id)}`);
          if (!state.active) return;
          job.textContent = `${value.status} · ${value.processed ?? value.next ?? "Unknown"} runs processed${value.error ? ` · ${value.error}` : ""}`;
          if (["complete", "completed", "failed", "cancelled"].includes(value.status)) clearInterval(timer);
        } catch (error) { job.textContent = `Cannot read import: ${error.message}`; }
      };
      clearInterval(timer); await poll(); timer = setInterval(poll, 2500);
    } catch (error) { job.textContent = `Cannot import retained runs: ${error.message}`; }
    finally { backfill.disabled = false; }
  }
  tabs.pricing = { show() { state.active = true; load(); }, hide() { state.active = false; clearInterval(timer); } };
})();
