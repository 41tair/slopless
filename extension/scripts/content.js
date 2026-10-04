(() => {
  if (location.hostname !== "github.com") return;
  let accounts = new Set();
  let level = "dim";
  let scope = { pr: true, issue: false };
  let showMarkCount = false;
  let markCounts = {};
  let debounce;
  let expiryTimer;
  let inFlight = false;
  let needsRefresh = true;
  let epoch = 0;
  let lastSignature = "";
  let nextCheckAt = Infinity;
  const selector = '[data-listview-component="items-list"] > li, .js-issue-row, [data-testid="list-row"], [data-slopless-level]';
  const authorSelector = '[data-testid="author-filter-link"], .opened-by .author';
  const style = document.createElement("style");
  style.textContent = `
    [data-slopless-level="hide"] { display: none !important; }
    [data-slopless-level="dim"] { opacity: .42; }
    [data-slopless-level="dim"]:hover, [data-slopless-level="dim"]:focus-within { opacity: 1; }
    .slopless-mark { display: inline-flex; align-items: center; margin-inline-start: 6px; padding: 1px 5px;
      border: 1px solid currentColor; border-radius: 3px; color: inherit; background: transparent;
      font: 500 10px/16px -apple-system, BlinkMacSystemFont, sans-serif; vertical-align: middle; }
  `;
  document.head.append(style);

  function pageAccounts() {
    const names = new Set();
    for (const row of document.querySelectorAll(selector)) {
      const username = (row.querySelector(authorSelector)?.textContent || "").trim().toLowerCase();
      if (/^[a-z\d][a-z\d-]{0,38}(?:\[bot\])?$/.test(username)) names.add(username);
    }
    return [...names].slice(0, 1000).map(username => ({ platform: "github", username }));
  }

  function render() {
    const list = location.pathname.match(/^\/[^/]+\/[^/]+\/(pulls|issues)\/?$/);
    const inScope = list && scope[list[1] === "pulls" ? "pr" : "issue"];
    for (const row of document.querySelectorAll(selector)) {
      const author = row.querySelector(authorSelector);
      const username = (author?.textContent || "").trim().toLowerCase();
      if (inScope && accounts.has(username)) {
        row.setAttribute("data-slopless-level", level);
        let badge = row.querySelector(".slopless-mark");
        if (!badge) {
          badge = document.createElement("span");
          badge.className = "slopless-mark";
          author.after(badge);
        }
        const count = markCounts[username];
        const label = showMarkCount && Number.isSafeInteger(count) && count > 0 ? `Marked ${count} times` : "Marked";
        if (badge.textContent !== label) badge.textContent = label;
        badge.title = "Slopless: marked account";
      } else {
        row.removeAttribute("data-slopless-level");
        row.querySelector(".slopless-mark")?.remove();
      }
    }
  }

  function schedule(force = false) {
    if (force) { epoch++; needsRefresh = true; }
    clearTimeout(debounce);
    debounce = setTimeout(refresh, 120);
  }

  async function refresh() {
    const targets = pageAccounts();
    const signature = location.pathname + JSON.stringify(targets);
    if (!needsRefresh && signature === lastSignature && Date.now() < nextCheckAt) { render(); return; }
    if (inFlight) { epoch++; needsRefresh = true; return; }
    needsRefresh = false;
    lastSignature = signature;
    clearTimeout(expiryTimer);
    const ticket = epoch;
    inFlight = true;
    try {
      const isList = /^\/[^/]+\/[^/]+\/(pulls|issues)\/?$/.test(location.pathname);
      const state = isList ? await chrome.runtime.sendMessage({ type: "slopless:query", accounts: targets }) : null;
      if (state?.error) throw new Error("Could not load filtering state.");
      if (ticket !== epoch || signature !== location.pathname + JSON.stringify(pageAccounts())) {
        needsRefresh = true;
        return;
      }
      accounts = new Set(state?.accounts || []);
      markCounts = state?.markCounts || {};
      if (state?.scope) scope = state.scope;
      if (["mark", "dim", "hide"].includes(state?.level)) level = state.level;
      showMarkCount = state?.showMarkCount === true;
      const deadlines = [state?.retryAt].filter(value => Number.isFinite(value) && value > Date.now());
      if (Number.isFinite(state?.expiresAt)) deadlines.push(state.expiresAt);
      nextCheckAt = deadlines.length ? Math.min(...deadlines) : Infinity;
      if (Number.isFinite(nextCheckAt)) expiryTimer = setTimeout(() => schedule(true), Math.max(1000, nextCheckAt - Date.now() + 100));
      render();
    } catch {
      accounts.clear();
      render();
      nextCheckAt = Date.now() + 30000;
      expiryTimer = setTimeout(() => schedule(true), 30000);
    } finally {
      inFlight = false;
      if (needsRefresh) schedule();
    }
  }

  chrome.runtime.onMessage.addListener(message => {
    if (message?.type === "slopless:refresh") schedule(true);
  });
  new MutationObserver(() => schedule()).observe(document.body, { childList: true, subtree: true, characterData: true });
  document.addEventListener("turbo:load", () => schedule(true));
  window.addEventListener("popstate", () => schedule(true));
  document.addEventListener("visibilitychange", () => { if (!document.hidden) schedule(true); });
  schedule(true);
})();
