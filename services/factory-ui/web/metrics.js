"use strict";
(() => {
  const body = measurementUI.panel("metrics", true), state = { data: null, hidden: new Set(), active: false, request: 0 };
  const metric = measurementUI.select("metric-name", [["", "Not set"], ["cost", "Cost"], ["duration_ms", "Runtime"], ["input_tokens", "Input tokens"], ["output_tokens", "Output tokens"], ["cached_input_tokens", "Cached input tokens"], ["uncached_input_tokens", "Uncached input tokens"], ["quality", "Quality metric"]]);
  const quality = h("input", { id: "metric-quality", hidden: true });
  const grouping = measurementUI.select("metric-group", [["", "Not set"], ["model", "Model"], ["workflow", "Workflow"], ["agent", "Agent"], ["run", "Run"]]);
  const range = measurementUI.select("metric-range", [["24h", "Last 24 hours"], ["7d", "Last 7 days"], ["30d", "Last 30 days"], ["custom", "Custom range"]]);
  range.value = "7d";
  const from = h("input", { id: "metric-from", type: "datetime-local", required: true });
  const to = h("input", { id: "metric-to", type: "datetime-local", required: true });
  const currency = measurementUI.select("metric-currency", [["", "Not set"], ["EUR", "EUR"], ["USD", "USD"]]);
  currency.value = localStorage.getItem("factory.displayCurrency") || "";
  const status = h("p", { className: "muted", role: "status" }), legend = h("div", { className: "measurement-legend" });
  const chart = h("div", { className: "measurement-chart" }), detail = h("div", { className: "measurement-table" });
  const findings = h("div", { className: "measurement-findings" });
  const qualityField = measurementUI.field("Quality metric name", quality); qualityField.hidden = true;
  metric.onchange = () => {
    quality.hidden = qualityField.hidden = metric.value !== "quality";
    const previous = grouping.value;
    const choices = metric.value === "duration_ms" ? [["workflow", "Workflow"], ["run", "Run"]]
      : metric.value === "quality" ? [["run", "Evaluated run"], ["agent", "Judge agent"]]
        : [["model", "Model"], ["workflow", "Workflow"], ["agent", "Agent"], ["run", "Run"]];
    grouping.replaceChildren(h("option", { value: "" }, "Not set"), ...choices.map(([value, label]) => h("option", { value }, label)));
    grouping.value = choices.some(([value]) => value === previous) ? previous : "";
    grouping.title = metric.value === "duration_ms" ? "Whole-workflow runtime has no single model or agent attribution" : metric.value === "quality" ? "Groups findings by evaluated run or producing judge" : "";
  };
  const fromField = measurementUI.field("From", from), toField = measurementUI.field("To", to);
  fromField.hidden = toField.hidden = true;
  const currencyField = measurementUI.field("Currency", currency); currencyField.hidden = true;
  function updateRange() {
    fromField.hidden = toField.hidden = range.value !== "custom";
    if (range.value === "custom") return;
    const end = new Date(), days = range.value === "24h" ? 1 : range.value === "30d" ? 30 : 7;
    const local = date => new Date(+date - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    from.value = local(new Date(+end - days * 86400000)); to.value = local(end);
  }
  const updateMetric = metric.onchange;
  metric.onchange = () => {
    updateMetric();
    if (!grouping.value) grouping.value = metric.value === "duration_ms" ? "workflow" : metric.value === "quality" ? "run" : "model";
    currencyField.hidden = metric.value !== "cost";
    if (state.active) load();
  };
  range.onchange = () => { updateRange(); if (state.active && range.value !== "custom") load(); };
  grouping.onchange = currency.onchange = () => { if (state.active) load(); };
  metric.value = "duration_ms"; metric.onchange(); updateRange();
  const submit = h("button", { type: "submit", className: "primary" }, "Apply");
  const form = h("form", { className: "measurement-filters", onsubmit: event => { event.preventDefault(); load(); } },
    measurementUI.field("Measurement", metric), qualityField, measurementUI.field("Group by", grouping), measurementUI.field("Period", range), fromField, toField, currencyField, submit);
  const valuesDetail = h("details", { className: "measurement-values" }, h("summary", {}, "Measured values"), detail);
  body.append(form, status, legend, chart, valuesDetail, findings);
  function svgNode(tag, attrs = {}, text) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
    if (text !== undefined) node.textContent = text;
    return node;
  }
  async function load() {
    if (!metric.value || !grouping.value || (metric.value === "quality" && !quality.value.trim())) { status.textContent = "Select a measurement and grouping."; return; }
    if (metric.value === "cost" && !currency.value) { status.textContent = "Select a currency."; return; }
    const start = new Date(from.value), end = new Date(to.value);
    if (!Number.isFinite(+start) || !Number.isFinite(+end) || start >= end) { status.textContent = "Select a valid time range."; return; }
    const request = ++state.request; submit.disabled = true;
    status.textContent = "Loading measurements…";
    chart.replaceChildren(); detail.replaceChildren(); legend.replaceChildren();
    loadFindings(start, end, request);
    try {
      const params = new URLSearchParams({ metric: metric.value === "quality" ? quality.value.trim() : metric.value, group_by: grouping.value, from: start.toISOString(), to: end.toISOString() });
      if (currency.value) params.set("currency", currency.value);
      const data = await api("GET", `/api/evaluations/series?${params}`);
      if (!state.active || request !== state.request) return;
      state.data = data; render();
    } catch (error) { if (request === state.request) status.textContent = `Cannot load graph: ${error.message}`; }
    finally { if (request === state.request) submit.disabled = false; }
  }
  async function loadFindings(start, end, request, cursor = "", append = false) {
    try {
      const params = new URLSearchParams({ kind: "judge", from: start.toISOString(), to: end.toISOString(), limit: "100" });
      if (cursor) params.set("cursor", cursor);
      const data = await api("GET", `/api/evaluations/measurements?${params}`);
      if (!state.active || request !== state.request) return;
      const rows = measurementUI.rows(data).filter(record => record.text_value !== undefined || record.status === "invalid" || record.status === "missing");
      const nodes = rows.map(record => h("details", { className: "measurement-finding" },
        h("summary", {}, record.metric_name || "Finding"),
        h("p", {}, record.text_value ?? ((record.missing || []).join("; ") || "Unavailable")),
        h("div", { className: "measurement-actions" },
          record.target_run_id ? h("a", { href: `#runs/${encodeURIComponent(record.target_run_id)}` }, "Evaluated run") : null,
          record.producer_run_id ? h("a", { href: `#runs/${encodeURIComponent(record.producer_run_id)}` }, "Judge run") : null,
          record.artifact_id ? h("a", { href: memoryObjectHash(record.artifact_id) }, "JSON output") : null)));
      if (!append) findings.replaceChildren(h("h2", {}, "Findings in this time range"));
      findings.querySelector?.("button")?.remove();
      findings.append(...nodes);
      if (data.next_cursor) findings.append(h("button", { type: "button", onclick: () => loadFindings(start, end, request, data.next_cursor, true) }, "More findings"));
      if (!rows.length && !data.next_cursor && !append) findings.replaceChildren();
    } catch (error) { if (state.active && request === state.request) findings.replaceChildren(errorPanel(`Cannot load findings: ${error.message}`)); }
  }
  function render() {
    const data = state.data; if (!data) return;
    const series = data.series || [];
    const projection = data.projection?.status;
    status.textContent = [projection && projection !== "complete" ? `Projection: ${projection}` : "", data.coverage ? `Known ${data.coverage.known ?? "unknown"} · Missing ${data.coverage.missing ?? "unknown"}` : "", data.truncated ? "Results capped" : ""].filter(Boolean).join(" · ");
    legend.replaceChildren(...series.map((line, index) => {
      const check = h("input", { type: "checkbox", checked: !state.hidden.has(line.key), onchange: () => { if (check.checked) state.hidden.delete(line.key); else state.hidden.add(line.key); render(); } });
      const total = line.exact_total && line.exact_complete ? h("span", { className: "muted", title: `Exact total: ${line.exact_total.numerator}/${line.exact_total.denominator}` }, ` · ${line.exact_total.decimal}`) : null;
      return h("label", { className: "measurement-series" }, check, h("span", { className: `series-${index % 6}`, "aria-hidden": "true" }, "━"), line.key || "Unattributed", total);
    }));
    const visible = series.filter(s => !state.hidden.has(s.key));
    const values = visible.flatMap(s => (s.points || []).filter(p => p.value !== null && p.value !== undefined && Number.isFinite(Number(p.value)) && Number.isFinite(Date.parse(p.time))));
    if (!values.length) { chart.replaceChildren(emptyPanel("No measured values")); detail.replaceChildren(); return; }
    const minX = Math.min(...values.map(p => Date.parse(p.time))), maxX = Math.max(...values.map(p => Date.parse(p.time)));
    const minY = Math.min(0, ...values.map(p => Number(p.value))), maxY = Math.max(...values.map(p => Number(p.value)));
    const x = t => 70 + ((Date.parse(t) - minX) / (maxX - minX || 1)) * 860;
    const y = v => 280 - ((Number(v) - minY) / (maxY - minY || 1)) * 250;
    const svg = svgNode("svg", { viewBox: "0 0 960 330", role: "img", "aria-label": `${metric.options[metric.selectedIndex].textContent} by ${grouping.value}` });
    svg.append(svgNode("path", { d: "M70 20 V280 H940", class: "plot-axis", fill: "none" }));
    for (let i = 0; i <= 4; i++) {
      const value = minY + (maxY - minY) * i / 4, top = y(value);
      svg.append(svgNode("path", { d: `M70 ${top} H940`, class: "plot-grid", fill: "none" }), svgNode("text", { x: 62, y: top + 4, "text-anchor": "end", class: "plot-label" }, Number(value.toPrecision(4)).toLocaleString()));
    }
    svg.append(svgNode("text", { x: 70, y: 310, class: "plot-label" }, new Date(minX).toLocaleString()), svgNode("text", { x: 940, y: 310, "text-anchor": "end", class: "plot-label" }, new Date(maxX).toLocaleString()));
    visible.forEach(line => {
      const index = series.indexOf(line); let path = "", gap = true;
      for (const p of line.points || []) {
        if (p.value === null || p.value === undefined || !Number.isFinite(Number(p.value)) || !Number.isFinite(Date.parse(p.time))) { gap = true; continue; }
        path += `${gap ? "M" : "L"}${x(p.time)} ${y(p.value)} `; gap = false;
        const dot = svgNode("circle", { cx: x(p.time), cy: y(p.value), r: 3, class: `plot-dot series-${index % 6}` });
        dot.append(svgNode("title", {}, `${line.key}: ${p.approximate ? "≈ " : ""}${p.value} · ${p.time}`)); svg.append(dot);
      }
      svg.append(svgNode("path", { d: path, fill: "none", class: `plot-line series-${index % 6}` }));
    });
    chart.replaceChildren(svg);
    detail.replaceChildren(dataTable(["Series", "Time", "Value", "State"], visible.flatMap(line => (line.points || []).slice(-100).map(p => h("tr", {}, h("td", {}, line.key), h("td", {}, p.time), h("td", { className: "num" }, p.value === null || p.value === undefined ? "Unknown" : String(p.value)), h("td", {}, p.status || "Reported"))))));
  }
  tabs.metrics = { show() { state.active = true; updateRange(); load(); }, hide() { state.active = false; ++state.request; submit.disabled = false; } };
})();
