"use strict";
(() => {
  const body = measurementUI.panel("pricing", true), state = { kind: "prices", current: null, active: false };
  const kind = measurementUI.select("pricing-kind", [["prices", "Model prices"], ["conversions", "Currency conversion"]]);
  const currency = measurementUI.select("pricing-display", [["", "Not set"], ["EUR", "EUR"], ["USD", "USD"]]);
  currency.value = localStorage.getItem("factory.displayCurrency") || "";
  currency.onchange = () => localStorage.setItem("factory.displayCurrency", currency.value);
  const list = h("div", { className: "measurement-list" }), editor = h("form", { className: "measurement-editor", hidden: true });
  const job = h("p", { role: "status", className: "muted" }); let timer = null;
  const add = iconButton("plus", "New price", () => edit({ id: crypto.randomUUID() }));
  const recalc = h("button", { type: "button", onclick: recalculate }, "Recalculate costs");
  const backfill = h("button", { type: "button", onclick: importRetained }, "Import retained runs");
  body.append(h("div", { className: "measurement-filters" }, measurementUI.field("Table", kind), measurementUI.field("Display currency", currency), add, recalc, backfill), list, editor, job);
  kind.onchange = () => { if (!editor.hidden) { kind.value = state.kind; notify("Finish or cancel the current edit", "error"); return; } state.kind = kind.value; add.title = state.kind === "prices" ? "New price" : "New conversion"; load(); };
  async function load() {
    try {
      const which = state.kind, data = await api("GET", `/api/evaluations/${which}`);
      if (!state.active || which !== state.kind) return;
      const items = measurementUI.rows(data);
      list.replaceChildren(...items.map(item => h("button", { type: "button", className: "measurement-list-row", onclick: () => edit(item) },
        h("span", {}, which === "prices" ? `${item.vendor} / ${item.model} / ${item.effort}` : `${item.from_currency} → ${item.to_currency}`),
        h("span", { className: "muted" }, which === "prices" ? `${item.from} · ${item.currency}` : item.rate))));
      if (!items.length) list.append(emptyPanel(which === "prices" ? "No prices" : "No conversion rates"));
    } catch (error) { list.replaceChildren(errorPanel(`Cannot load prices: ${error.message}`)); }
  }
  function edit(item) {
    state.current = item; editor.hidden = false; const controls = {};
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
    editor.replaceChildren(...fields, problem, h("div", { className: "measurement-actions" }, h("button", { type: "button", onclick: () => { editor.hidden = true; state.current = null; } }, "Cancel"), save));
    editor.onsubmit = async event => {
      event.preventDefault(); const value = { ...item };
      try {
        for (const [key, input] of Object.entries(controls)) value[key] = input.value.trim();
        if (state.kind === "prices") {
          value.modes = JSON.parse(modes.value);
          if (!value.modes || Array.isArray(value.modes) || typeof value.modes !== "object") throw new Error("Modes must be a JSON object.");
          if (!open.checked && !value.to) throw new Error("Enter an end date or select No end date.");
          if (!/(Z|[+-]\d\d:\d\d)$/.test(value.from) || (!open.checked && !/(Z|[+-]\d\d:\d\d)$/.test(value.to))) throw new Error("Effective dates must include Z or an explicit timezone offset.");
          value.from = new Date(value.from).toISOString(); value.to = open.checked ? null : new Date(value.to).toISOString();
          value.unit_tokens = Number(value.unit_tokens);
          if (!Number.isSafeInteger(value.unit_tokens) || value.unit_tokens <= 0) throw new Error("Tokens per rate must be a positive integer.");
        }
        save.disabled = true;
        await measurementUI.write(`/api/evaluations/${state.kind}/${encodeURIComponent(item.id)}`, value, item.revision);
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
