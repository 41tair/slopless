import assert from "node:assert/strict";
import test from "node:test";

import {
  assertSingleReadOnlyStatement,
  decodeParameter,
  handleD1Request,
  jsonSerializable
} from "../src/d1.mjs";

function jsonRequest(path, body, method = "POST") {
  return new Request(`http://d1.internal${path}`, {
    method,
    headers: { "content-type": "application/json" },
    body: method === "POST" ? JSON.stringify(body) : undefined
  });
}

function database({ batchResult = [], rawResult = [[]], failure } = {}) {
  const calls = [];
  return {
    calls,
    prepare(sql) {
      const statement = {
        sql,
        params: [],
        bind(...params) {
          this.params = params;
          return this;
        },
        async raw(options) {
          calls.push({ operation: "raw", sql: this.sql, params: this.params, options });
          if (failure) throw failure;
          return rawResult;
        }
      };
      return statement;
    },
    async batch(statements) {
      calls.push(...statements.map((statement) => ({
        operation: "batch",
        sql: statement.sql,
        params: statement.params
      })));
      if (failure) throw failure;
      return batchResult;
    }
  };
}

test("query uses one native D1 batch and decodes REST BLOB parameters", async () => {
  const db = database({
    batchResult: [{ success: true, results: [{ id: 1, payload: new Uint8Array([4, 5]) }], meta: { changes: 0 } }]
  });
  const response = await handleD1Request(jsonRequest("/query", {
    batch: [{ sql: "SELECT ?", params: [{ blob: "00AFFE" }] }]
  }), db);

  assert.equal(response.status, 200);
  assert.deepEqual(Array.from(db.calls[0].params[0]), [0, 175, 254]);
  assert.deepEqual(await response.json(), {
    result: [{ success: true, results: [{ id: 1, payload: [4, 5] }], meta: { changes: 0 } }],
    success: true,
    errors: [],
    messages: []
  });
});

test("raw preserves columns and row order", async () => {
  const db = database({ rawResult: [["b", "a"], [2, 1], [4, 3]] });
  const response = await handleD1Request(jsonRequest("/raw", {
    sql: "SELECT b, a FROM sample WHERE id = ?",
    params: [7]
  }), db);

  assert.equal(response.status, 200);
  assert.deepEqual(db.calls, [{
    operation: "raw",
    sql: "SELECT b, a FROM sample WHERE id = ?",
    params: [7],
    options: { columnNames: true }
  }]);
  assert.deepEqual((await response.json()).result[0].results, {
    columns: ["b", "a"],
    rows: [[2, 1], [4, 3]]
  });
});

test("raw allows semicolons in literals but rejects a second statement", () => {
  assert.doesNotThrow(() => assertSingleReadOnlyStatement("SELECT ';' AS value; -- done"));
  assert.doesNotThrow(() => assertSingleReadOnlyStatement("PRAGMA table_info(\"users\")"));
  assert.throws(() => assertSingleReadOnlyStatement("SELECT 1; DELETE FROM users"), /one statement/);
  assert.throws(() => assertSingleReadOnlyStatement("WITH gone AS (DELETE FROM users) SELECT 1"), /read-only/);
  assert.throws(() => assertSingleReadOnlyStatement("PRAGMA user_version = 2"), /read-only/);
});

test("D1 failures expose only a numeric SQLite code", async () => {
  const db = database({ failure: new Error("secret@example.com UNIQUE failed: SQLITE_CONSTRAINT") });
  const response = await handleD1Request(jsonRequest("/query", {
    batch: [{ sql: "INSERT INTO users(email) VALUES (?)", params: ["secret@example.com"] }]
  }), db);
  const encoded = await response.text();

  assert.equal(response.status, 409);
  assert.equal(encoded.includes("secret@example.com"), false);
  assert.deepEqual(JSON.parse(encoded).errors, [{ code: 19, message: "D1 constraint failed" }]);
});

test("permanent SQLite errors do not use a retryable HTTP status", async () => {
  for (const marker of ["SQLITE_ERROR", "SQLITE_SCHEMA", "SQLITE_FULL"]) {
    const db = database({ failure: new Error(`D1_ERROR: rejected: ${marker}`) });
    const response = await handleD1Request(jsonRequest("/query", {
      batch: [{ sql: "SELECT broken", params: [] }]
    }), db);

    assert.equal(response.status, 400, marker);
    assert.equal((await response.json()).success, false);
  }
});

test("transient and unclassified binding errors remain retryable", async () => {
  for (const failure of [
    new Error("D1_ERROR: database is busy: SQLITE_BUSY"),
    new Error("binding transport disconnected")
  ]) {
    const db = database({ failure });
    const response = await handleD1Request(jsonRequest("/query", {
      batch: [{ sql: "SELECT 1", params: [] }]
    }), db);
    const body = await response.json();

    assert.equal(response.status, 503);
    assert.equal(body.errors[0].code, failure.message.includes("SQLITE_BUSY") ? 5 : 1);
    assert.equal(JSON.stringify(body).includes(failure.message), false);
  }
});

test("adapter rejects unsupported routes and parameter objects", async () => {
  const db = database();
  assert.throws(() => decodeParameter({ blob: "abc" }), /invalid blob/);
  assert.deepEqual(jsonSerializable(new Uint8Array([1, 2])), [1, 2]);

  const route = await handleD1Request(jsonRequest("/other", {}), db);
  assert.equal(route.status, 404);
  const method = await handleD1Request(jsonRequest("/query", {}, "GET"), db);
  assert.equal(method.status, 405);
});
