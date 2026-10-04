import { DEFAULT_SETTINGS, readSettings, saveSettings, validateSettings, permissionOrigin } from "./store.js";

const $ = id => document.getElementById(id);
let saved = null;
let saving = false;

function fields() {
  return {
    endpoint: $("endpoint").value,
    apiKey: $("api-key").value,
    level: document.querySelector('input[name="level"]:checked')?.value || DEFAULT_SETTINGS.level,
    scope: { pr: $("scope-pr").checked, issue: $("scope-issue").checked },
    showMarkCount: $("show-mark-count").checked,
  };
}

function fill(settings) {
  $("endpoint").value = settings.endpoint;
  $("api-key").value = settings.apiKey;
  document.querySelector(`input[name="level"][value="${settings.level}"]`).checked = true;
  $("scope-pr").checked = settings.scope.pr;
  $("scope-issue").checked = settings.scope.issue;
  $("show-mark-count").checked = settings.showMarkCount;
}

function setFeedback(message, error = false) {
  $("settings-feedback").textContent = message;
  $("settings-feedback").hidden = !message;
  $("settings-feedback").classList.toggle("error", error);
}

function dirtyState() {
  const changed = JSON.stringify(fields()) !== JSON.stringify(saved);
  $("save-state").textContent = changed ? "Unsaved changes" : "Saved";
  return changed;
}

$("settings-form").addEventListener("input", () => { setFeedback(""); dirtyState(); });
$("settings-form").addEventListener("submit", async event => {
  event.preventDefault();
  if (saving || !saved) return;
  saving = true;
  $("settings-fields").disabled = true;
  $("save-state").textContent = "Saving…";
  setFeedback("");
  try {
    const settings = validateSettings(fields());
    const allowed = await chrome.permissions.request({ origins: [permissionOrigin(settings.endpoint)] });
    if (!allowed) throw new Error("Allow access to the API endpoint to save these settings.");
    saved = await saveSettings(settings);
    fill(saved);
  } catch (error) {
    setFeedback(error.message, true);
  } finally {
    saving = false;
    $("settings-fields").disabled = false;
    dirtyState();
  }
});

$("toggle-key").addEventListener("click", () => {
  const show = $("api-key").type === "password";
  $("api-key").type = show ? "text" : "password";
  $("toggle-key").textContent = show ? "Hide" : "Show";
  $("toggle-key").setAttribute("aria-label", show ? "Hide API token" : "Show API token");
  $("toggle-key").setAttribute("aria-pressed", String(show));
});

$("reset-settings").addEventListener("click", () => {
  fill(DEFAULT_SETTINGS);
  dirtyState();
  setFeedback("Defaults restored. Save to apply. Server marks are kept.");
});

window.addEventListener("beforeunload", event => {
  if (saved && JSON.stringify(fields()) !== JSON.stringify(saved)) {
    event.preventDefault();
    event.returnValue = "";
  }
});

async function initialize() {
  try {
    saved = await readSettings();
    fill(saved);
    $("settings-fields").disabled = false;
    dirtyState();
  } catch {
    $("save-state").textContent = "Could not load settings";
    setFeedback("Open Slopless settings from your browser's extensions page and try again.", true);
  }
}
initialize();
