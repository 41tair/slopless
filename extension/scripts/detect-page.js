// Injected into the active tab when the popup opens. Return value goes to the popup.
(() => {
  const url = new URL(location.href);
  const unsupported = { status: "unsupported" };
  if (url.origin !== "https://github.com") return unsupported;
  const validLogin = value => /^[a-z\d][a-z\d-]{0,38}(?:\[bot\])?$/i.test(value || "");
  const path = url.pathname.replace(/\/$/, "");
  const match = path.match(/^\/([^/]+)\/([^/]+)\/(pull|issues)\/(\d+)(?:\/.*)?$/);

  function loginFromLink(element) {
    if (!element) return null;
    try {
      const href = new URL(element.getAttribute("href"), url);
      if (href.origin !== url.origin) return null;
      const login = decodeURIComponent(href.pathname.slice(1));
      if (validLogin(login)) return login;
      const text = element.textContent.trim();
      if (href.pathname.startsWith("/apps/") && validLogin(text) && text.endsWith("[bot]")) return text;
    } catch { /* Ignore malformed author links. */ }
    return null;
  }

  if (match) {
    const [, owner, repo, route, number] = match;
    const selectors = route === "issues"
      ? ['[data-testid="issue-body-header-author"]', '.js-issue .gh-header-meta a.author', '.gh-header-meta a.author']
      : ['.gh-header-meta a.author', '[data-testid="pull-request-header"] a.author'];
    let login = selectors.map(selector => loginFromLink(document.querySelector(selector))).find(Boolean);
    if (!login) {
      // Match metadata URL first: GitHub SPA navigation can leave stale metadata.
      const canonical = document.querySelector('meta[property="og:url"]')?.content;
      const metadataLogin = document.querySelector('meta[property="og:author:username"]')?.content;
      try {
        const canonicalURL = new URL(canonical);
        if (canonicalURL.origin === url.origin && canonicalURL.pathname === `/${owner}/${repo}/${route}/${number}` && validLogin(metadataLogin)) login = metadataLogin;
      } catch { /* No matching metadata. */ }
    }
    const title = (document.querySelector('[data-testid="issue-title"], .js-issue-title, .markdown-title')?.textContent || document.title.split(" · ")[0]).trim();
    return {
      status: login ? "found" : "missing-author", kind: route === "pull" ? "pr" : "issue",
      login, number, repository: `${owner}/${repo}`, title,
      url: `https://github.com/${owner}/${repo}/${route}/${number}`,
    };
  }

  const profile = path.match(/^\/([^/]+)$/);
  if (profile) {
    let login;
    try { login = decodeURIComponent(profile[1]); } catch { return unsupported; }
    // A one-segment URL may be /settings, /explore, or an organization.
    const nickname = document.querySelector('.vcard-username, [itemprop="additionalName"]')?.textContent.trim();
    if (validLogin(login) && nickname?.toLowerCase() === login.toLowerCase()) {
      return { status: "found", kind: "profile", login: nickname, title: "Profile", url: `https://github.com/${login}` };
    }
  }
  return unsupported;
})();
