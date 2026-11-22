"use strict";
const measurementUI = {
  field(label, control) { return h("div", { className: "field" }, h("label", { htmlFor: control.id }, label), control); },
  select(id, options) { return h("select", { id }, ...options.map(([value, label]) => h("option", { value }, label))); },
  nav(section = "evaluations", active = "evaluations") {
    const metrics = section === "metrics";
    const links = metrics ? [["metrics", "Graphs"], ["pricing", "Prices"]] : [["evaluations", "Sets"], ["judges", "Judges"]];
    return h("nav", { className: "measurement-nav", "aria-label": metrics ? "Metrics" : "Evaluations" }, ...links.map(([id, label]) => h("a", { href: `#${id}`, "aria-current": id === active ? "page" : null }, label)));
  },
  panel(id, navigation = false) {
    const page = $(`tab-${id}`), body = h("div", { className: "panel-body measurement-body" });
    const section = id === "metrics" || id === "pricing" ? "metrics" : "evaluations";
    const title = { metrics: "Graphs", pricing: "Prices", judges: "Judge configuration", events: "Events" }[id];
    if (navigation) page.classList.add("page-sectioned");
    page.replaceChildren(...(navigation ? [this.nav(section, id)] : []),
      h("div", { className: "panel content-panel" }, h("div", { className: "panel-head" }, h("h2", {}, title)), body));
    return body;
  },
  rows(data) { return Array.isArray(data) ? data : (data.items || data.measurements || data.prices || data.conversions || data.bindings || []); },
  async write(path, value, revision) { return api("PUT", path, value, revision ? { "If-Match": revision.startsWith('"') ? revision : `"${revision}"` } : { "If-None-Match": "*" }); },
};
const evaluationPage = $("tab-evaluations");
evaluationPage.classList.add("page-sectioned");
evaluationPage.prepend(measurementUI.nav("evaluations", "evaluations"));
