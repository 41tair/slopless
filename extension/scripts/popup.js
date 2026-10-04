import { LEVELS, normalizeLogin, readSettings } from "./store.js";

const $ = id => document.getElementById(id);
let context = null;
let tabId = null;
let state = null;
let busy = false;
let refreshWanted = false;
let expiryTimer;
let viewEpoch = 0;

function feedback(message, error = false) {
  $("feedback").textContent = message;
  $("feedback").classList.toggle("error", error);
  $("feedback").hidden = !message;
}

function errorMessage(error) {
  const seconds = Math.ceil(((error?.retryAt || 0) - Date.now()) / 1000);
  return `${error?.message || "Could not load account status. Try again."}${seconds > 0 ? ` Retry in ${seconds} seconds.` : ""}`;
}

function showEmpty(title, description) {
  context = null;
  $("loading").hidden = true;
  $("account-card").hidden = true;
  $("mark-action").hidden = true;
  $("empty-state").hidden = false;
  $("empty-title").textContent = title;
  $("empty-description").textContent = description;
}

function currentAccount() {
  return state?.data?.find(item => item.username === normalizeLogin(context?.login))?.state;
}

function pendingHere() {
  return state?.pending?.accounts?.some(account => account.platform === "github" && account.username === normalizeLogin(context?.login));
}

function renderState() {
  if (!state) return;
  const scope = state.settings.scope;
  const scopeLabel = [scope.pr && "PRs", scope.issue && "issues"].filter(Boolean).join(" and ");
  $("current-level").textContent = scopeLabel ? `${LEVELS[state.settings.level].label} · ${scopeLabel}` : "Paused";
  if (!context) return;
  const account = currentAccount();
  const mine = account?.markedByMe === true;
  const pending = pendingHere();
  $("action-title").textContent = !state.configured ? "API token required" : pending ? "Confirm pending mark" : !account ? "Status unavailable" : mine ? "Unmark this account?" : "Mark this account?";
  $("action-description").textContent = !state.configured ? "Add your API token in Settings to check and mark accounts."
    : pending ? state.pending.expired ? "Refresh status before choosing a new action." : "Retry the previous request to confirm its result."
    : state.pending ? `Finish the pending mark for @${state.pending.accounts[0].username} before starting another action.`
    : !account ? "Refresh status to check this account with the API."
    : mine ? "Remove your mark. Other users' marks may still filter this account."
    : account.marked ? "This account is marked by the community. Add your mark."
    : "Add your mark for this account.";
  $("toggle-mark").textContent = busy ? "Please wait…" : pending ? "Retry mark" : mine ? "Unmark account" : "Mark account";
  $("toggle-mark").classList.toggle("primary", !mine);
  $("toggle-mark").classList.toggle("secondary", mine);
  $("toggle-mark").disabled = busy || !state.configured || (state.pending ? !pending || state.pending.expired : !account)
    || Boolean(state.error?.retryAt > Date.now() || state.markRetryAt > Date.now());
  $("refresh-status").disabled = busy;
}

function scheduleRefresh() {
  clearTimeout(expiryTimer);
  const deadlines = [state?.error?.retryAt, state?.markRetryAt].filter(time => Number.isFinite(time) && time > Date.now());
  if (Number.isFinite(state?.expiresAt)) deadlines.push(state.expiresAt);
  if (deadlines.length) expiryTimer = setTimeout(() => refreshStatus(), Math.max(1000, Math.min(...deadlines) - Date.now() + 100));
}

function renderContext(detected) {
  context = detected;
  $("loading").hidden = true;
  $("empty-state").hidden = true;
  $("account-card").hidden = false;
  $("mark-action").hidden = false;
  const labels = { pr: "Pull request", issue: "Issue", profile: "Profile" };
  $("page-type").textContent = `${labels[context.kind]}${context.number ? ` · #${context.number}` : ""}`;
  $("author-label").textContent = context.kind === "profile" ? "Account" : "Author";
  $("profile-link").textContent = `@${context.login}`;
  $("profile-link").href = `https://github.com/${encodeURIComponent(context.login)}`;
  $("repository").hidden = !context.repository;
  $("repository").textContent = context.repository || "";
  $("page-title").textContent = context.title;
  $("page-title").title = context.title;
  renderState();
}

async function detect(tab) {
  const results = await chrome.scripting.executeScript({ target: { tabId: tab }, files: ["scripts/detect-page.js"] });
  return results[0]?.result;
}

async function loadStatus(force = false) {
  const ticket = viewEpoch;
  const response = await chrome.runtime.sendMessage({ type: "slopless:query", force,
    accounts: [{ platform: "github", username: normalizeLogin(context.login) }] });
  if (ticket !== viewEpoch) { refreshWanted = true; return; }
  if (!response?.settings) throw new Error(response?.error?.message || "Could not load account status.");
  state = response;
  if (state.error) feedback(errorMessage(state.error), true);
  else if (!(state.markRetryAt > Date.now()) && $("feedback").classList.contains("error")) feedback("");
  scheduleRefresh();
}

async function refreshStatus(force = false) {
  if (!context) return;
  if (busy) { refreshWanted = true; return; }
  busy = true;
  renderState();
  try {
    if (force) feedback("");
    await loadStatus(force);
  } catch (error) { feedback(error.message, true); }
  finally {
    busy = false;
    renderState();
    if (refreshWanted) { refreshWanted = false; refreshStatus(); }
  }
}

async function initialize() {
  if (busy) return;
  busy = true;
  viewEpoch++;
  context = null;
  feedback("");
  $("loading").hidden = false;
  $("empty-state").hidden = true;
  $("account-card").hidden = true;
  $("mark-action").hidden = true;
  try {
    state = { settings: await readSettings(), data: [], configured: false };
    renderState();
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab?.id || !tab.url?.startsWith("https://github.com/")) {
      showEmpty("Open a GitHub page", "Open a pull request, issue, or profile, then click the Slopless icon.");
      return;
    }
    tabId = tab.id;
    const detected = await detect(tabId);
    if (detected?.status === "found" && normalizeLogin(detected.login)) {
      renderContext(detected);
      await loadStatus();
    } else if (detected?.status === "missing-author") showEmpty("Author not found", "Wait for the page to finish loading, then try again.");
    else showEmpty("Page not supported", "Open a pull request, issue, or profile to mark an account.");
  } catch (error) {
    if (!context) showEmpty("Could not read this page", "Refresh the GitHub page, then try again.");
    feedback(error.message, true);
  } finally {
    busy = false;
    renderState();
    if (refreshWanted) { refreshWanted = false; refreshStatus(); }
  }
}

$("toggle-mark").addEventListener("click", async () => {
  if (busy || !context || !state || (!currentAccount() && !pendingHere())) return;
  busy = true;
  viewEpoch++;
  renderState();
  feedback("");
  try {
    const current = await detect(tabId);
    if (current?.status !== "found" || current.url !== context.url || normalizeLogin(current.login) !== normalizeLogin(context.login)) {
      showEmpty("Page changed", "Read the page again before marking an account.");
      return;
    }
    const marked = pendingHere() ? state.pending.marked : !currentAccount().markedByMe;
    const accounts = pendingHere() ? state.pending.accounts : [{ platform: "github", username: normalizeLogin(context.login) }];
    const response = await chrome.runtime.sendMessage({ type: "slopless:mark", accounts, marked });
    if (!response?.settings) throw new Error(response?.error?.message || "Could not save your mark.");
    state = response;
    if (response.error) feedback(`${response.saved ? "Your mark was saved, but status could not be refreshed. " : ""}${errorMessage(response.error)}`, true);
    else feedback(marked ? "Your mark was saved." : currentAccount()?.marked ? "Your mark was removed. Other users still mark this account." : "Your mark was removed.");
    scheduleRefresh();
  } catch (error) { feedback(error.message, true); }
  finally {
    busy = false;
    renderState();
    if (refreshWanted) { refreshWanted = false; refreshStatus(); }
  }
});

$("open-settings").addEventListener("click", async () => {
  try { await chrome.runtime.openOptionsPage(); } catch { feedback("Could not open Settings.", true); }
});
$("retry").addEventListener("click", initialize);
$("refresh-status").addEventListener("click", () => refreshStatus(true));
chrome.runtime.onMessage.addListener(message => {
  if (message?.type !== "slopless:refresh") return;
  viewEpoch++;
  refreshStatus();
});
initialize();
