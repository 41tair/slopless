export const LEVELS = {
  mark: { label: "Mark only" },
  dim: { label: "Dim" },
  hide: { label: "Hide" },
};
export const DEFAULT_SETTINGS = {
  endpoint: "https://slopless.byron.fun/openapi/v1", apiKey: "", level: "dim",
  scope: { pr: true, issue: false }, showMarkCount: false,
};

export function normalizeLogin(value) {
  const login = typeof value === "string" ? value.trim() : "";
  return /^[a-z\d][a-z\d-]{0,38}(?:\[bot\])?$/i.test(login) ? login.toLowerCase() : null;
}

export function normalizeSettings(value = {}) {
  let endpoint = typeof value?.endpoint === "string" ? value.endpoint.trim().replace(/\/+$/, "") : "";
  if (!endpoint || endpoint === "https://slopless.byron.fun/v1") endpoint = DEFAULT_SETTINGS.endpoint;
  return {
    endpoint,
    apiKey: typeof value?.apiKey === "string" ? value.apiKey.trim() : "",
    level: Object.hasOwn(LEVELS, value?.level) ? value.level : DEFAULT_SETTINGS.level,
    scope: {
      pr: typeof value?.scope?.pr === "boolean" ? value.scope.pr : DEFAULT_SETTINGS.scope.pr,
      issue: typeof value?.scope?.issue === "boolean" ? value.scope.issue : DEFAULT_SETTINGS.scope.issue,
    },
    showMarkCount: typeof value?.showMarkCount === "boolean" ? value.showMarkCount : DEFAULT_SETTINGS.showMarkCount,
  };
}

export function validateSettings(value) {
  const settings = normalizeSettings(value);
  let url;
  try { url = new URL(settings.endpoint); } catch {
    throw new Error(`Enter a complete API endpoint, such as ${DEFAULT_SETTINGS.endpoint}.`);
  }
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if ((url.protocol !== "https:" && !(url.protocol === "http:" && local)) || url.username || url.password || url.hash || url.search) {
    throw new Error("Use an HTTPS endpoint without credentials, a query, or a # fragment. HTTP is allowed for localhost.");
  }
  if (/[\r\n]/.test(settings.apiKey)) throw new Error("Enter a valid API token.");
  settings.endpoint = url.href.replace(/\/+$/, "");
  return settings;
}

export function permissionOrigin(endpoint) {
  const url = new URL(endpoint);
  return `${url.protocol}//${url.hostname}/*`;
}

export async function readSettings() {
  const { settings } = await chrome.storage.local.get("settings");
  return normalizeSettings(settings);
}

export async function saveSettings(value) {
  const settings = validateSettings(value);
  await chrome.storage.local.set({ settings });
  return settings;
}
