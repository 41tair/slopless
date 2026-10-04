import { normalizeLogin, normalizeSettings, validateSettings, permissionOrigin } from "./store.js";

export const CACHE_TTL_MS = 5 * 60 * 1000;
export const CACHE_PREFIX = "slopless:api:v1:";
export const IDEMPOTENCY_TTL_MS = 24 * 60 * 60 * 1000;
const MAX_ENTRIES = 1000;
const TIMEOUT_MS = 10000;

export class ApiError extends Error {
  constructor(code, message, { status = 0, retryAt = null, uncertain = false } = {}) {
    super(message);
    Object.assign(this, { code, status, retryAt, uncertain });
  }
}

export function publicError(error) {
  return {
    code: error.code || "request_failed", message: error instanceof ApiError ? error.message : "Could not load account status. Try again.",
    status: error.status || 0, retryAt: error.retryAt || null,
  };
}

export function normalizeAccounts(input, maximum = 1000) {
  if (!Array.isArray(input) || input.length < 1 || input.length > maximum) {
    throw new ApiError("invalid_accounts", `Provide between 1 and ${maximum} accounts.`);
  }
  const accounts = new Map();
  for (const item of input) {
    const username = normalizeLogin(item?.username);
    if (item?.platform !== "github" || !username) throw new ApiError("invalid_accounts", "Provide valid GitHub account names.");
    accounts.set(`github:${username}`, { platform: "github", username });
  }
  return [...accounts.values()];
}

export const accountKey = account => `${account.platform}:${account.username}`;

export function validateData(body, requested, marked) {
  if (!Array.isArray(body?.data) || body.data.length !== requested.length) throw invalidResponse();
  const expected = new Set(requested.map(accountKey));
  const records = new Map();
  for (const item of body.data) {
    const key = accountKey(item || {});
    if (!expected.has(key) || records.has(key) || normalizeLogin(item.username) !== item.username
      || typeof item.marked !== "boolean" || typeof item.markedByMe !== "boolean"
      || !Number.isSafeInteger(item.markCount) || item.markCount < 0
      || item.marked !== (item.markCount > 0) || (item.markedByMe && !item.marked)
      || (typeof marked === "boolean" && item.markedByMe !== marked)) throw invalidResponse();
    records.set(key, {
      platform: item.platform, username: item.username, marked: item.marked,
      markCount: item.markCount, markedByMe: item.markedByMe,
    });
  }
  return requested.map(account => records.get(accountKey(account)));
}

function invalidResponse() {
  return new ApiError("invalid_response", "The API returned an invalid account response.", { uncertain: true });
}

function retryDelay(response, body, now) {
  const header = response.headers.get("Retry-After");
  const seconds = header && /^\d+$/.test(header) ? Number(header) : body?.retryAfterSeconds;
  if (Number.isFinite(seconds) && seconds > 0) return Math.min(seconds * 1000, 7 * 86400000);
  const date = header ? Date.parse(header) : NaN;
  if (Number.isFinite(date) && date > now) return Math.min(date - now, 7 * 86400000);
  return response.status === 429 ? 60000 : 5000;
}

// A single background-owned service serializes requests within each credential namespace.
// Durable state contains no token and survives service-worker restarts.
export function createAccountService({ storage, fetch: fetcher = globalThis.fetch, now = Date.now,
  uuid = () => crypto.randomUUID(), digest = async text => {
    const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
    return [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, "0")).join("");
  }, hasPermission = async () => true } = {}) {
  const contexts = new Map();

  async function context() {
    const raw = await storage.get("settings");
    const settings = normalizeSettings(raw.settings);
    const namespace = await digest(JSON.stringify([settings.endpoint, settings.apiKey]));
    if (!contexts.has(namespace)) {
      contexts.set(namespace, (async () => {
        const key = CACHE_PREFIX + namespace;
        const saved = (await storage.get(key))[key];
        const entries = {};
        for (const [id, entry] of Object.entries(saved?.entries || {}).slice(-MAX_ENTRIES)) {
          try {
            const account = normalizeAccounts([entry.account])[0];
            const [valid] = validateData({ data: [entry.account] }, [account]);
            if (id === accountKey(account) && Number.isFinite(entry.fetchedAt) && entry.fetchedAt <= now()) {
              entries[id] = { account: valid, fetchedAt: entry.fetchedAt };
            }
          } catch { /* Old or corrupt records are not authoritative. */ }
        }
        return { key, namespace, settings, entries, pending: saved?.pending || null,
          cooldowns: saved?.cooldowns || {}, queue: Promise.resolve() };
      })());
    }
    const ctx = await contexts.get(namespace);
    ctx.settings = settings;
    return ctx;
  }

  async function persist(ctx) {
    const entries = Object.entries(ctx.entries).sort((a, b) => b[1].fetchedAt - a[1].fetchedAt).slice(0, MAX_ENTRIES);
    ctx.entries = Object.fromEntries(entries);
    await storage.set({ [ctx.key]: { entries: ctx.entries, pending: ctx.pending, cooldowns: ctx.cooldowns } });
  }

  function serial(ctx, work) {
    const result = ctx.queue.then(work);
    ctx.queue = result.catch(() => {});
    return result;
  }

  function fresh(entry) {
    return entry && entry.fetchedAt <= now() && entry.fetchedAt + CACHE_TTL_MS > now();
  }

  async function ensureActive(ctx) {
    if ((await context()).namespace !== ctx.namespace) throw new ApiError("settings_changed", "API settings changed. Check the account again.");
  }

  async function request(ctx, route, payload, key) {
    await ensureActive(ctx);
    if (!ctx.settings.apiKey) throw new ApiError("not_configured", "Add an API token in Settings to check and mark accounts.");
    try { validateSettings(ctx.settings); } catch {
      throw new ApiError("invalid_settings", "Update the API endpoint in Settings.");
    }
    if (!await hasPermission(permissionOrigin(ctx.settings.endpoint))) {
      throw new ApiError("permission_required", "Open Settings and save to allow access to the API endpoint.");
    }
    const cooldown = ctx.cooldowns[route];
    if (cooldown?.retryAt > now()) throw new ApiError(cooldown.code, cooldown.message, cooldown);
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), TIMEOUT_MS);
    let response;
    let body;
    try {
      response = await fetcher(`${ctx.settings.endpoint}/accounts/${route}`, {
        method: "POST", credentials: "omit", redirect: "error", cache: "no-store", signal: controller.signal,
        headers: { Authorization: `Bearer ${ctx.settings.apiKey}`, "Content-Type": "application/json", Accept: "application/json",
          ...(key ? { "Idempotency-Key": key } : {}) },
        body: JSON.stringify(payload),
      });
      body = await response.json().catch(() => null);
    } catch {
      const error = new ApiError("network_error", "The API could not be reached. Try again.", { retryAt: now() + 5000, uncertain: true });
      ctx.cooldowns[route] = { ...publicError(error), uncertain: error.uncertain };
      await persist(ctx);
      throw error;
    } finally {
      clearTimeout(timeout);
    }
    if (response.status !== 200) {
      const retryable = response.status === 429 || response.status >= 500 || (response.status === 409 && body?.code === "idempotency_in_progress");
      const message = response.status === 401 ? "The API token is invalid or expired. Update it in Settings."
        : response.status === 403 ? "The API token does not have permission for this operation."
        : response.status === 429 ? "API rate limit reached. Try again after the cooldown."
        : response.status === 409 ? "The previous mark is still being resolved. Check its status before trying again."
        : `The API request failed (${response.status}).`;
      const error = new ApiError(response.status === 429 ? "rate_limited" : response.status === 401 ? "unauthorized" : response.status === 403 ? "forbidden" : "api_error", message, {
        status: response.status, retryAt: now() + (retryable ? retryDelay(response, body, now()) : 60000),
        uncertain: response.status >= 500 || (response.status === 409 && body?.code === "idempotency_in_progress"),
      });
      ctx.cooldowns[route] = { ...publicError(error), uncertain: error.uncertain };
      if (response.status === 429 && body?.rateLimit?.policy?.startsWith("openapi.")) {
        ctx.cooldowns.query = ctx.cooldowns.mark = publicError(error);
      }
      await persist(ctx);
      throw error;
    }
    delete ctx.cooldowns[route];
    let records;
    try { records = validateData(body, payload.accounts, payload.marked); } catch (error) {
      error.retryAt = now() + 30000;
      ctx.cooldowns[route] = { ...publicError(error), uncertain: error.uncertain };
      await persist(ctx);
      throw error;
    }
    return { records, replayed: response.headers.get("Idempotency-Replayed") === "true" };
  }

  function cache(ctx, records) {
    const fetchedAt = now();
    for (const account of records) ctx.entries[accountKey(account)] = { account, fetchedAt };
  }

  async function queryBatch(ctx, accounts) {
    const { records } = await request(ctx, "query", { accounts });
    cache(ctx, records);
    // An expired uncertain operation is never replayed. A fresh query confirms its current state.
    if (ctx.pending && now() - ctx.pending.createdAt >= IDEMPOTENCY_TTL_MS
      && ctx.pending.accounts.every(account => fresh(ctx.entries[accountKey(account)]))) ctx.pending = null;
    await persist(ctx);
  }

  async function snapshot(input = []) {
    const ctx = await context();
    const accounts = input.length ? normalizeAccounts(input) : [];
    const data = accounts.map(account => {
      const entry = ctx.entries[accountKey(account)];
      return { ...account, state: fresh(entry) ? entry.account : null, stale: Boolean(entry && !fresh(entry)) };
    });
    const cached = Object.values(ctx.entries).filter(fresh);
    const pending = ctx.pending ? {
      accounts: ctx.pending.accounts, marked: ctx.pending.marked,
      expired: now() - ctx.pending.createdAt >= IDEMPOTENCY_TTL_MS,
    } : null;
    return { settings: ctx.settings, configured: Boolean(ctx.settings.apiKey), data, pending,
      markRetryAt: ctx.cooldowns.mark?.retryAt || null,
      cached: cached.map(entry => entry.account),
      expiresAt: cached.length ? Math.min(...cached.map(entry => entry.fetchedAt + CACHE_TTL_MS)) : null };
  }

  async function query(input, { force = false } = {}) {
    const accounts = normalizeAccounts(input);
    const ctx = await context();
    let error = null;
    try {
      await serial(ctx, async () => {
        const missing = accounts.filter(account => force || !fresh(ctx.entries[accountKey(account)]));
        for (let index = 0; index < missing.length; index += 100) await queryBatch(ctx, missing.slice(index, index + 100));
      });
      await ensureActive(ctx);
    } catch (failure) { error = publicError(failure); }
    return { ...await snapshot(accounts), error };
  }

  async function mark(input, marked) {
    const accounts = normalizeAccounts(input, 50);
    if (typeof marked !== "boolean") throw new ApiError("invalid_mark", "Choose whether to mark the account.");
    const ctx = await context();
    let refreshError = null;
    await serial(ctx, async () => {
      await ensureActive(ctx);
      const pending = ctx.pending;
      if (pending && now() - pending.createdAt >= IDEMPOTENCY_TTL_MS) {
        throw new ApiError("operation_expired", "The pending mark expired. Refresh account status before choosing a new action.");
      }
      const fingerprint = JSON.stringify({ accounts, marked });
      if (pending && JSON.stringify({ accounts: pending.accounts, marked: pending.marked }) !== fingerprint) {
        throw new ApiError("operation_pending", "Retry the pending mark before starting another marking action.");
      }
      if (!pending) {
        ctx.pending = { accounts, marked, key: uuid(), createdAt: now() };
      }
      for (const account of accounts) delete ctx.entries[accountKey(account)];
      await persist(ctx);
      let result;
      try {
        result = await request(ctx, "mark", { accounts: ctx.pending.accounts, marked }, ctx.pending.key);
      } catch (error) {
        // Definitive rejection permits a new decision. Uncertain writes retain their exact key.
        if (error instanceof ApiError && !error.uncertain && error.status !== 429 && error.code !== "rate_limited") {
          ctx.pending = null;
          await persist(ctx);
        }
        throw error;
      }
      const completed = ctx.pending;
      ctx.pending = null;
      if (result.replayed) {
        for (const account of accounts) delete ctx.entries[accountKey(account)];
        try { await persist(ctx); } catch (error) { ctx.pending = completed; throw error; }
        try { await queryBatch(ctx, accounts); } catch (error) { refreshError = publicError(error); }
      } else {
        cache(ctx, result.records);
        try { await persist(ctx); } catch (error) {
          ctx.pending = completed;
          for (const account of accounts) delete ctx.entries[accountKey(account)];
          throw error;
        }
      }
    });
    await ensureActive(ctx);
    return { ...await snapshot(accounts), saved: true, error: refreshError };
  }

  return { query, mark, snapshot };
}
