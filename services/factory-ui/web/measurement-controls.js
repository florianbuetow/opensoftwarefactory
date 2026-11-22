"use strict";
const measurementUI = {
  field(label, control) { return h("div", { className: "field" }, h("label", { htmlFor: control.id }, label), control); },
  select(id, options) { return h("select", { id }, ...options.map(([value, label]) => h("option", { value }, label))); },
  nav() { return h("nav", { className: "measurement-nav", "aria-label": "Evaluations" }, ...[["evaluations", "Sets"], ["metrics", "Graphs"], ["judges", "Judges"], ["pricing", "Prices"]].map(([id, label]) => h("a", { href: `#${id}` }, label))); },
  panel(id, evaluation = false) {
    const page = $(`tab-${id}`), body = h("div", { className: "panel-body measurement-body" });
    page.replaceChildren(h("div", { className: "panel content-panel" }, evaluation ? this.nav() : null, body));
    return body;
  },
  rows(data) { return Array.isArray(data) ? data : (data.items || data.measurements || data.prices || data.conversions || data.bindings || []); },
  async write(path, value, revision) { return api("PUT", path, value, revision ? { "If-Match": revision.startsWith('"') ? revision : `"${revision}"` } : { "If-None-Match": "*" }); },
};
$("evaluation-new-slot").parentElement.insertAdjacentElement("afterend", measurementUI.nav());
