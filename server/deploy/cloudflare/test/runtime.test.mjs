import assert from "node:assert/strict";
import test from "node:test";

import { ContainerProxy, ServerContainer } from "../src/index.mjs";

test("the real ContainerProxy registry dispatches d1.internal to the binding adapter", async () => {
  const calls = [];
  const db = {
    prepare(sql) {
      return {
        bind(...params) {
          return { sql, params };
        }
      };
    },
    async batch(statements) {
      calls.push(statements);
      return [{ success: true, results: [{ value: 1 }], meta: { changes: 0 } }];
    }
  };
  const proxy = new ContainerProxy({
    props: {
      className: ServerContainer.name,
      containerId: "test",
      enableInternet: false,
      allowedHosts: ["d1.internal"],
      interceptAll: false
    }
  }, { DB: db });

  const response = await proxy.fetch(new Request("http://d1.internal/query", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ batch: [{ sql: "SELECT 1", params: [] }] })
  }));

  assert.equal(response.status, 200);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], [{ sql: "SELECT 1", params: [] }]);
  assert.equal((await response.json()).success, true);
});
