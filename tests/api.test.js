import assert from "node:assert/strict";
import { test } from "node:test";
import { createAccountService, validateData, CACHE_PREFIX, CACHE_TTL_MS, IDEMPOTENCY_TTL_MS } from "../extension/scripts/api.js";
import { DEFAULT_SETTINGS } from "../extension/scripts/store.js";

const account = username => ({ platform: "github", username });
const record = (username, markCount = 0, markedByMe = false) => ({ ...account(username), marked: markCount > 0, markCount, markedByMe });
const response = (data, headers = {}) => new Response(JSON.stringify({ data }), { status: 200, headers });
const failure = (status, body = {}, headers = {}) => new Response(JSON.stringify(body), { status, headers });
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; };

function setup(fetch) {
  const data = { settings: { ...DEFAULT_SETTINGS, apiKey: "secret-token" } };
  let clock = 1000000;
  const calls = [];
  const storage = {
    get: async key => key === null ? structuredClone(data) : { [key]: structuredClone(data[key]) },
    set: async value => Object.assign(data, structuredClone(value)),
  };
  const options = { storage, now: () => clock, hasPermission: async () => true,
    fetch: async (url, options) => {
      const call = { url, ...options, body: JSON.parse(options.body) };
      calls.push(call);
      return fetch ? fetch(call, calls.length) : response(call.body.accounts.map(item => record(item.username)));
    } };
  return { data, calls, storage, options, service: createAccountService(options), advance: delta => { clock += delta; } };
}

test("queries follow OpenAPI and ignore all legacy mock marks", async () => {
  const h = setup();
  h.data["account:aipd506"] = { marked: true, markCount: 99 };
  assert.deepEqual((await h.service.snapshot()).cached, []);
  const view = await h.service.query([account(" AIPD506 "), account("aipd506")]);
  assert.equal(h.calls.length, 1);
  const call = h.calls[0];
  assert.equal(call.url, "https://slopless.byron.fun/openapi/v1/accounts/query");
  assert.equal(call.method, "POST");
  assert.equal(call.headers.Authorization, "Bearer secret-token");
  assert.equal(call.credentials, "omit");
  assert.equal(call.redirect, "error");
  assert.deepEqual(call.body, { accounts: [account("aipd506")] });
  assert.deepEqual(view.data[0].state, record("aipd506"));
  assert.equal(Object.keys(h.data).filter(key => key.startsWith(CACHE_PREFIX)).length, 1);
  assert.equal(Object.keys(h.data).some(key => key.includes("secret-token")), false);
});

test("query batching, negative caching, concurrent coalescing, and worker restart persistence", async () => {
  const h = setup();
  const targets = Array.from({ length: 201 }, (_, index) => account(`user-${index}`));
  await Promise.all([h.service.query(targets), h.service.query(targets)]);
  assert.deepEqual(h.calls.map(call => call.body.accounts.length), [100, 100, 1]);
  const restarted = createAccountService(h.options);
  assert.equal((await restarted.query([targets[0]])).data[0].state.marked, false);
  assert.equal(h.calls.length, 3);
  h.advance(CACHE_TTL_MS + 1);
  await restarted.query([targets[0]]);
  assert.equal(h.calls.length, 4);
});

test("expired cache and failed requests stay unknown rather than creating negative results", async () => {
  let offline = false;
  const h = setup(call => { if (offline) throw new Error("offline"); return response(call.body.accounts.map(item => record(item.username, 3))); });
  await h.service.query([account("alice")]);
  const persisted = JSON.stringify(Object.values(h.data).find(value => value?.entries)?.entries);
  h.advance(CACHE_TTL_MS + 1);
  offline = true;
  const view = await h.service.query([account("alice")]);
  assert.equal(view.data[0].state, null);
  assert.equal(view.data[0].stale, true);
  assert.equal(view.error.code, "network_error");
  assert.equal(JSON.stringify(Object.values(h.data).find(value => value?.entries)?.entries), persisted);
  await h.service.query([account("alice")]);
  assert.equal(h.calls.length, 2);
  h.advance(5001);
  offline = false;
  assert.equal((await h.service.query([account("alice")])).data[0].state.markCount, 3);
});

test("endpoint and token changes isolate cached state, including late in-flight responses", async () => {
  const started = deferred();
  const release = deferred();
  const h = setup(async (call, number) => {
    if (number === 1) { started.resolve(); await release.promise; }
    return response(call.body.accounts.map(item => record(item.username, number === 1 ? 9 : 0)));
  });
  const old = h.service.query([account("alice")]);
  await started.promise;
  h.data.settings = { ...h.data.settings, apiKey: "different-token" };
  release.resolve();
  const oldView = await old;
  assert.equal(oldView.error.code, "settings_changed");
  assert.equal(oldView.data[0].state, null);
  assert.equal((await h.service.query([account("alice")])).data[0].state.markCount, 0);
  h.data.settings = { ...h.data.settings, endpoint: "https://other.example/custom" };
  assert.equal((await h.service.snapshot([account("alice")])).data[0].state, null);
  await h.service.query([account("alice")]);
  assert.equal(h.calls[2].url, "https://other.example/custom/accounts/query");
});

test("invalid responses cannot poison cache or invent unmarked results", async () => {
  for (const data of [[], [record("other")], [record("alice"), record("alice")],
    [{ ...record("alice"), markCount: -1 }], [{ ...record("alice"), marked: true }],
    [{ ...record("alice"), markedByMe: true }], [{ ...record("alice"), markCount: "3" }]]) {
    assert.throws(() => validateData({ data }, [account("alice")]));
  }
  const h = setup(() => response([]));
  const view = await h.service.query([account("alice")]);
  assert.equal(view.error.code, "invalid_response");
  assert.equal(view.data[0].state, null);
  assert.deepEqual(view.cached, []);
});

test("mark and withdrawal use markedByMe while community marks remain", async () => {
  const h = setup(call => response([record("alice", call.body.marked === true ? 4 : 3, call.body.marked === true)]));
  let view = await h.service.mark([account("alice")], true);
  assert.equal(view.saved, true);
  assert.equal(view.data[0].state.markedByMe, true);
  assert.equal(view.data[0].state.markCount, 4);
  assert.match(h.calls[0].headers["Idempotency-Key"], /^[a-z\d-]{36}$/i);
  view = await h.service.mark([account("alice")], false);
  assert.equal(view.data[0].state.markedByMe, false);
  assert.equal(view.data[0].state.marked, true);
  assert.equal(view.data[0].state.markCount, 3);
  assert.notEqual(h.calls[0].headers["Idempotency-Key"], h.calls[1].headers["Idempotency-Key"]);
  assert.equal(h.calls[1].body.marked, false);
  await assert.rejects(h.service.mark(Array.from({ length: 51 }, (_, n) => account(`user-${n}`)), true));
});

test("an older query cannot overwrite a successful mark", async () => {
  const started = deferred();
  const release = deferred();
  const h = setup(async call => {
    if (call.url.endsWith("/query")) { started.resolve(); await release.promise; return response([record("alice")]); }
    return response([record("alice", 1, true)]);
  });
  const query = h.service.query([account("alice")]);
  await started.promise;
  const mark = h.service.mark([account("alice")], true);
  release.resolve();
  await Promise.all([query, mark]);
  assert.deepEqual((await h.service.snapshot([account("alice")])).data[0].state, record("alice", 1, true));
});

test("uncertain writes retain a durable key and replay results trigger a fresh query", async () => {
  const h = setup((call, number) => {
    if (number === 1) throw new Error("response lost after commit");
    if (call.url.endsWith("/mark")) return response([record("alice", 5, true)], { "Idempotency-Replayed": "true" });
    return response([record("alice", 2, false)]);
  });
  await assert.rejects(h.service.mark([account("alice")], true));
  assert.equal((await h.service.snapshot()).pending.marked, true);
  await assert.rejects(h.service.mark([account("alice")], false), /pending mark/);
  h.advance(5001);
  const restarted = createAccountService(h.options);
  const view = await restarted.mark([account("alice")], true);
  assert.equal(h.calls[0].headers["Idempotency-Key"], h.calls[1].headers["Idempotency-Key"]);
  assert.equal(h.calls[2].url.endsWith("/query"), true);
  assert.deepEqual(view.data[0].state, record("alice", 2, false));
  assert.equal(view.pending, null);
});

test("a replay with a failed refresh is saved but cannot create a fresh historical cache entry", async () => {
  const h = setup(call => call.url.endsWith("/mark") ? response([record("alice", 7, true)], { "Idempotency-Replayed": "true" }) : failure(503, {}, { "Retry-After": "10" }));
  const view = await h.service.mark([account("alice")], true);
  assert.equal(view.saved, true);
  assert.equal(view.data[0].state, null);
  assert.equal(view.pending, null);
  assert.equal(view.error.status, 503);
});

test("Retry-After is persistent, blocks repeated requests, and preserves pending mark keys", async () => {
  const h = setup((call, number) => number === 1 ? failure(429, { code: "rate_limited" }, { "Retry-After": "60" }) : response([record("alice", 1, true)]));
  await assert.rejects(h.service.mark([account("alice")], true), error => error.status === 429);
  const restarted = createAccountService(h.options);
  const status = await restarted.snapshot([account("alice")]);
  assert.equal(status.markRetryAt, 1060000);
  assert.equal(status.pending.marked, true);
  await assert.rejects(restarted.mark([account("alice")], true), error => error.status === 429);
  assert.equal(h.calls.length, 1);
  h.advance(60001);
  await restarted.mark([account("alice")], true);
  assert.equal(h.calls.length, 2);
  assert.equal(h.calls[0].headers["Idempotency-Key"], h.calls[1].headers["Idempotency-Key"]);
});

test("expired uncertain writes are never automatically replayed", async () => {
  const h = setup((call, number) => { if (number === 1) throw new Error("offline"); return response([record("alice", 1, true)]); });
  await assert.rejects(h.service.mark([account("alice")], true));
  h.advance(IDEMPOTENCY_TTL_MS + 1);
  await assert.rejects(h.service.mark([account("alice")], true), /expired/);
  assert.equal(h.calls.length, 1);
  const view = await h.service.query([account("alice")], { force: true });
  assert.equal(view.pending, null);
  assert.equal(h.calls[1].url.endsWith("/query"), true);
});

test("missing token, denied endpoint permission, and revoked credentials do not create local marks", async () => {
  const h = setup(() => failure(401, { code: "unauthorized" }));
  h.data.settings.apiKey = "";
  assert.equal((await h.service.query([account("alice")])).error.code, "not_configured");
  assert.equal(h.calls.length, 0);
  h.data.settings.apiKey = "secret-token";
  const denied = createAccountService({ ...h.options, hasPermission: async () => false });
  assert.equal((await denied.query([account("alice")])).error.code, "permission_required");
  assert.equal(h.calls.length, 0);
  await assert.rejects(h.service.mark([account("alice")], true), error => error.status === 401);
  const view = await h.service.snapshot([account("alice")]);
  assert.equal(view.data[0].state, null);
  assert.equal(view.pending, null);
});

test("pending keys must reach storage before a mutation is sent", async () => {
  const h = setup(call => response([record("alice", 1, true)]));
  const realSet = h.storage.set;
  let failing = true;
  h.storage.set = async value => { if (failing) throw new Error("disk full"); return realSet(value); };
  await assert.rejects(h.service.mark([account("alice")], true));
  assert.equal(h.calls.length, 0);
  failing = false;
  await h.service.mark([account("alice")], true);
  assert.equal(h.calls.length, 1);
});
