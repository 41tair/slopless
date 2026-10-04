import { createAccountService, publicError } from "./api.js";
import { readSettings } from "./store.js";

const ready = chrome.storage.local.setAccessLevel({ accessLevel: "TRUSTED_CONTEXTS" });
const service = createAccountService({
  storage: chrome.storage.local,
  hasPermission: origin => chrome.permissions.contains({ origins: [origin] }),
});

function publicSettings(settings) {
  const { level, scope, showMarkCount } = settings;
  return { level, scope, showMarkCount };
}

function popupView(view) {
  return {
    settings: publicSettings(view.settings), configured: view.configured, data: view.data,
    pending: view.pending, markRetryAt: view.markRetryAt, expiresAt: view.expiresAt,
    error: view.error || null, saved: view.saved || false,
  };
}

function filterView(view) {
  const marked = view.data.map(item => item.state).filter(item => item?.marked);
  return {
    ...publicSettings(view.settings), accounts: marked.map(item => item.username),
    markCounts: view.settings.showMarkCount ? Object.fromEntries(marked.map(item => [item.username, item.markCount])) : {},
    expiresAt: view.expiresAt, retryAt: view.error?.retryAt || null,
  };
}

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (sender.id !== chrome.runtime.id) return;
  const extensionPage = ["index.html", "settings.html"].some(path => sender.url === chrome.runtime.getURL(path));
  let listKind = null;
  try {
    const url = new URL(sender.url);
    if (url.origin === "https://github.com") {
      const match = url.pathname.match(/^\/[^/]+\/[^/]+\/(pulls|issues)\/?$/);
      if (match) listKind = match[1] === "pulls" ? "pr" : "issue";
    }
  } catch { /* Ignore unsupported senders. */ }
  if (!["slopless:query", "slopless:mark"].includes(message?.type)) return;
  if (!extensionPage && (!listKind || message.type !== "slopless:query")) return;

  (async () => {
    await ready;
    try {
      let view;
      if (message.type === "slopless:mark") {
        view = await service.mark(message.accounts, message.marked);
      } else {
        const settings = await readSettings();
        const enabled = extensionPage || settings.scope[listKind];
        view = enabled && message.accounts?.length
          ? await service.query(message.accounts, { force: extensionPage && message.force === true })
          : await service.snapshot(message.accounts || []);
      }
      return extensionPage ? popupView(view) : filterView(view);
    } catch (error) {
      if (extensionPage) {
        const view = await service.snapshot(message.accounts || []).catch(() => null);
        return view ? { ...popupView(view), error: publicError(error) } : { error: publicError(error) };
      }
      return { ...filterView(await service.snapshot()), retryAt: error.retryAt || null };
    }
  })().then(sendResponse, () => sendResponse({ error: { code: "storage_error", message: "Could not read local storage. Try again." } }));
  return true;
});

let refreshTimer;
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== "local" || !Object.keys(changes).some(key => key === "settings" || key.startsWith("slopless:api:v1:"))) return;
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(async () => {
    const tabs = await chrome.tabs.query({ url: "https://github.com/*" });
    await Promise.allSettled([
      ...tabs.map(tab => chrome.tabs.sendMessage(tab.id, { type: "slopless:refresh" })),
      chrome.runtime.sendMessage({ type: "slopless:refresh" }),
    ]);
  }, 50);
});
