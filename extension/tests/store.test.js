import assert from "node:assert/strict";
import { test } from "node:test";
import { DEFAULT_SETTINGS, normalizeLogin, normalizeSettings, validateSettings, permissionOrigin, readSettings, saveSettings } from "../scripts/store.js";

test("GitHub usernames are case insensitive and support bot accounts", () => {
  assert.equal(normalizeLogin("  Develop-KIM "), "develop-kim");
  assert.equal(normalizeLogin("renovate[bot]"), "renovate[bot]");
  for (const value of ["", "../settings", "a/b", "<script>", null]) assert.equal(normalizeLogin(value), null);
});

test("missing settings use Dim, PR only, and the new public API endpoint", () => {
  assert.deepEqual(normalizeSettings(null), DEFAULT_SETTINGS);
  assert.equal(normalizeSettings({ level: "toString" }).level, "dim");
  assert.equal(normalizeSettings({ level: "hide" }).level, "hide");
  assert.equal(normalizeSettings({ endpoint: "https://slopless.byron.fun/v1/" }).endpoint, DEFAULT_SETTINGS.endpoint);
  assert.equal(normalizeSettings({ endpoint: "https://custom.example/v1" }).endpoint, "https://custom.example/v1");
});

test("scope and count display preserve explicit choices", () => {
  assert.deepEqual(normalizeSettings({ scope: { pr: false, issue: true } }).scope, { pr: false, issue: true });
  assert.deepEqual(normalizeSettings({ scope: { pr: "false", issue: null } }).scope, DEFAULT_SETTINGS.scope);
  assert.equal(normalizeSettings({ showMarkCount: true }).showMarkCount, true);
  assert.equal(normalizeSettings({ showMarkCount: "true" }).showMarkCount, false);
});

test("endpoints preserve custom paths and only allow HTTPS or local HTTP", () => {
  assert.equal(validateSettings({ endpoint: " http://localhost:8080/api/v1/ " }).endpoint, "http://localhost:8080/api/v1");
  assert.equal(validateSettings({ endpoint: "https://example.com/custom/v2/" }).endpoint, "https://example.com/custom/v2");
  assert.equal(permissionOrigin("http://localhost:8080/api/v1"), "http://localhost/*");
  for (const endpoint of ["invalid", "javascript:alert(1)", "http://example.com", "https://user:pass@example.com", "https://example.com/#secret", "https://example.com/?version=2"]) {
    assert.throws(() => validateSettings({ endpoint }));
  }
  assert.throws(() => validateSettings({ apiKey: "token\nInjected: header" }));
});

test("settings save without modifying legacy sample data", async () => {
  const data = { "account:old-user": { marked: true, markCount: 99 } };
  globalThis.chrome = { storage: { local: {
    get: async () => structuredClone(data),
    set: async value => Object.assign(data, structuredClone(value)),
  } } };
  await saveSettings({ ...DEFAULT_SETTINGS, apiKey: " token ", scope: { pr: false, issue: true } });
  const settings = await readSettings();
  assert.equal(settings.apiKey, "token");
  assert.deepEqual(settings.scope, { pr: false, issue: true });
  assert.deepEqual(data["account:old-user"], { marked: true, markCount: 99 });
});
