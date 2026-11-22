"use strict";
(() => {
  const body = $("system-diagram");
  let loaded = false, request = 0;

  async function load() {
    const current = ++request;
    body.replaceChildren(emptyPanel("Loading system diagram…"));
    try {
      const response = await fetch("system-diagram.mmd", { cache: "no-store" });
      if (!response.ok) throw new Error(`the diagram file answered ${response.status}`);
      const source = await response.text();
      if (current !== request) return;
      if (!source.trim()) throw new Error("the diagram file is empty");
      body.replaceChildren(InteractiveSession.renderMarkdown(`\`\`\`mermaid\n${source}\n\`\`\``));
      loaded = true;
    } catch (error) {
      if (current !== request) return;
      body.replaceChildren(errorPanel(`Cannot load system diagram: ${error.message}`));
    }
  }

  tabs["system-diagram"] = {
    show() { if (!loaded) load(); },
    hide() { ++request; },
  };
})();
