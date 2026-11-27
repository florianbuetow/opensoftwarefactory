import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
const definitions = app.slice(app.indexOf("const PAGES = ["), app.indexOf("// Icons:"));
const navigation = app.slice(app.indexOf("const pageHrefs = new Map();"), app.indexOf('window.addEventListener("resize", fitMenu);'));

function setup() {
  const listeners = {};
  const document = { activeElement: null, addEventListener(type, callback) { listeners[type] = callback; } };
  class Element {
    constructor(tag, props = {}, children = []) {
      this.tag = tag; this.children = children; this.dataset = props.dataset || {}; this.attrs = {};
      this.className = props.className || ""; this.hidden = !!props.hidden; this.style = {};
      this.title = props.title;
      this.onclick = props.onclick; this.parentElement = null; this.clientWidth = 600; this.scrollWidth = 400;
      this.classList = { add() {}, remove() {}, toggle() {} };
      for (const [key, value] of Object.entries(props)) if (key.startsWith("aria-")) this.attrs[key] = value;
    }
    append(...children) { this.children.push(...children); }
    replaceChildren(...children) { this.children = children; }
    setAttribute(key, value) { this.attrs[key] = value; }
    getBoundingClientRect() { return { left: 100, bottom: 56, width: 220 }; }
    querySelectorAll(selector) {
      if (selector === "a") return this.children.filter((child) => child.tag === "a");
      if (selector === ".nav-trigger") return this.children.filter((child) => child.className === "nav-trigger");
      return [];
    }
    querySelector(selector) {
      if (selector === "a") return this.querySelectorAll("a")[0] || null;
      if (selector === '[aria-current="page"]') return this.querySelectorAll("a").find((child) => child.attrs["aria-current"] === "page") || null;
      return null;
    }
    contains(node) { return this === node || this.children.includes(node); }
    focus() { document.activeElement = this; }
  }
  const rail = new Element("nav"), bar = new Element("header"); rail.parentElement = bar;
  document.body = new Element("body");
  const context = { document, window: { innerWidth: 1200, addEventListener() {} }, activeTab: "metrics",
    h: (tag, props, ...children) => new Element(tag, props, children), icon: () => new Element("span"), $: () => rail,
    Map, location: { hash: "#metrics" }, decisionsState: { pending: [{}, {}] }, sessionsState: { pending: [] } };
  vm.createContext(context);
  vm.runInContext(`${definitions}\n${navigation}\nglobalThis.nav = { buildRail, openNavPopover, closeNavPopover, updateNavCounts, groups: NAV_GROUPS, pages: PAGES, getPopover: () => navPopover };`, context);
  context.nav.buildRail();
  return { context, rail, listeners, document };
}

test("each page has one navigation group and groups open by click", () => {
  const { context, rail, listeners, document } = setup();
  const pages = Array.from(context.nav.pages, (page) => page.name);
  assert.equal(context.nav.pages.find((page) => page.name === "artifacts").title, "Blobstore");
  const grouped = Array.from(context.nav.groups.flatMap((group) => group.pages));
  assert.deepEqual(grouped.slice().sort(), pages.slice().sort());
  assert.equal(new Set(grouped).size, pages.length);
  assert.deepEqual(rail.children.map((button) => button.dataset.group), ["Main", "Factory", "Tasks", "Connectors", "Context"]);

  const tasks = rail.children[2];
  tasks.onclick();
  const popover = context.nav.getPopover();
  assert.equal(popover.hidden, false);
  assert.equal(tasks.attrs["aria-expanded"], "true");
  assert.deepEqual(popover.children.map((link) => link.dataset.page), ["decisions", "sessions"]);
  // Each Tasks page carries its own count of what waits, hidden at none, and follows the polls while the menu is open.
  const counts = () => popover.children.map((link) => { const b = link.children.find((c) => c?.dataset?.count); return [b.dataset.count, b.textContent, b.hidden, b.title]; });
  assert.deepEqual(counts(), [["decisions", "2", false, "2 gates waiting"], ["sessions", "", true, "0 sessions waiting"]]);
  context.decisionsState.pending = [];
  context.sessionsState.pending = [{}];
  context.nav.updateNavCounts();
  assert.deepEqual(counts(), [["decisions", "", true, "0 gates waiting"], ["sessions", "1", false, "1 session waiting"]]);
  listeners.focusin({ target: rail });
  assert.equal(popover.hidden, true);
  tasks.onclick();
  listeners.keydown({ key: "Escape", preventDefault() {} });
  assert.equal(popover.hidden, true);
  assert.equal(document.activeElement, tasks);

  rail.children[4].onclick();
  assert.deepEqual(popover.children.map((item) => item.dataset.page || item.className), ["policies", "skills", "nav-planned", "artifacts"]);
  const planned = popover.children.find((item) => item.className === "nav-planned");
  assert.equal(planned.attrs["aria-disabled"], "true");
  assert.equal(planned.children[1].children[0], "Memory");
  assert.equal(planned.children.length, 2);
  assert.equal(planned.title, "Planned feature");
  assert.equal(planned.href, undefined);
});

// The System Diagram page has its own tests in system_diagram_ui_test.mjs.
