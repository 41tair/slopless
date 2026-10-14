import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  checkWorkerAccess,
  deploymentPlan,
  main,
  parseArguments,
  renderConfig
} from "../deploy.mjs";

const values = Object.freeze({
  IMAGE: "ghcr.io/example/slopless",
  COMMIT: "0123456789abcdef0123456789abcdef01234567",
  CLOUDFLARE_ACCOUNT_ID: "0123456789abcdef0123456789abcdef",
  CLOUDFLARE_D1_DATABASE_ID: "00000000-0000-0000-0000-000000000000"
});

function apiResponse(status, body) {
  return {
    status,
    async json() {
      return body;
    }
  };
}

test("deployment plan uses the existing commit image and immutable Cloudflare tag", () => {
  const options = parseArguments(["--dry-run"], values);
  assert.deepEqual(deploymentPlan(options), {
    sourceImage: "ghcr.io/example/slopless:sha-0123456789abcdef0123456789abcdef01234567",
    localImage: "slopless-server:sha-0123456789abcdef0123456789abcdef01234567",
    deployedImage: "registry.cloudflare.com/0123456789abcdef0123456789abcdef/slopless-server:sha-0123456789abcdef0123456789abcdef01234567"
  });
});

test("rendered config selects default scheduling and the immutable image", async () => {
  const options = parseArguments(["--dry-run"], values);
  const template = await readFile(new URL("../wrangler.template.json", import.meta.url), "utf8");
  const config = JSON.parse(renderConfig(template, options));

  assert.equal(config.account_id, values.CLOUDFLARE_ACCOUNT_ID);
  assert.equal(config.workers_dev, false);
  assert.equal(config.preview_urls, false);
  assert.deepEqual(config.routes, [{ pattern: "slopless.byron.fun", custom_domain: true }]);
  assert.match(config.containers[0].image, /:sha-0123456789abcdef0123456789abcdef01234567$/);
  assert.equal(config.containers[0].scheduling_policy, "default");
  assert.equal(config.containers[0].max_instances, 1);
  assert.equal(config.d1_databases[0].database_id, values.CLOUDFLARE_D1_DATABASE_ID);
  assert.equal(config.d1_databases[0].migrations_dir, "./migrations");
  assert.equal(config.vars.CONTAINER_SLEEP_AFTER, "10m");
});

test("dry run is offline and never prints the API token", async () => {
  const messages = [];
  const token = "must-not-appear-anywhere";
  await main(["--dry-run"], { ...values, CLOUDFLARE_API_TOKEN: token }, {
    log(message) {
      messages.push(message);
    }
  });

  assert.equal(messages.length, 1);
  assert.equal(messages.join("\n").includes(token), false);
  assert.equal(JSON.parse(messages[0]).dryRun, true);
});

test("invalid inputs fail before any deployment command", () => {
  assert.throws(
    () => parseArguments(["--dry-run"], { ...values, COMMIT: "latest" }),
    /full 40-character Git SHA/
  );
  assert.throws(
    () => parseArguments(["--dry-run", "--unknown"], values),
    /unknown option/
  );
});

test("Worker access preflight is a read-only authenticated request", async () => {
  const requests = [];
  const token = "preflight-token-must-stay-private";
  await checkWorkerAccess({
    workerName: "slopless-server",
    accountID: values.CLOUDFLARE_ACCOUNT_ID,
    token,
    async fetchImpl(url, init) {
      requests.push({ url, init });
      return apiResponse(200, { success: true, errors: [], result: [] });
    }
  });

  assert.equal(requests.length, 1);
  assert.equal(
    requests[0].url,
    "https://api.cloudflare.com/client/v4/accounts/0123456789abcdef0123456789abcdef/workers/scripts/slopless-server/secrets"
  );
  assert.equal(requests[0].url.includes("/d1/"), false);
  assert.equal(requests[0].init.method, "GET");
  assert.equal(requests[0].init.headers.Authorization, `Bearer ${token}`);
  assert.equal(requests[0].init.signal.aborted, false);
});

test("Worker access preflight permits Cloudflare's explicit new-Worker errors", async () => {
  for (const code of [10007, 10090]) {
    await checkWorkerAccess({
      workerName: "new-worker",
      accountID: values.CLOUDFLARE_ACCOUNT_ID,
      token: "private-token",
      fetchImpl: async () => apiResponse(400, {
        success: false,
        errors: [{ code, message: "worker does not exist" }]
      })
    });
  }
});

test("Worker access preflight never masks authorization failures as a missing Worker", async () => {
  const token = "authorization-token-must-stay-private";
  for (const status of [401, 403]) {
    await assert.rejects(
      checkWorkerAccess({
        workerName: "slopless-server",
        accountID: values.CLOUDFLARE_ACCOUNT_ID,
        token,
        fetchImpl: async () => apiResponse(status, {
          success: false,
          errors: [{ code: 10007, message: "masked" }]
        })
      }),
      (error) => {
        assert.match(error.message, /Workers Scripts Write/);
        assert.match(error.message, /Workers Admin/);
        assert.equal(error.message.includes(token), false);
        assert.equal(error.message.includes("masked"), false);
        return true;
      }
    );
  }

  await assert.rejects(
    checkWorkerAccess({
      workerName: "slopless-server",
      accountID: values.CLOUDFLARE_ACCOUNT_ID,
      token,
      fetchImpl: async () => apiResponse(400, {
        success: false,
        errors: [{ code: 10090 }, { code: 9106 }]
      })
    }),
    /Workers Scripts Write/
  );
});

test("Worker access preflight distinguishes API and network failures without leaking details", async () => {
  const token = "diagnostic-token-must-stay-private";
  await assert.rejects(
    checkWorkerAccess({
      workerName: "slopless-server",
      accountID: values.CLOUDFLARE_ACCOUNT_ID,
      token,
      fetchImpl: async () => ({
        status: 503,
        async json() {
          throw new SyntaxError("raw upstream response must stay private");
        }
      })
    }),
    (error) => {
      assert.match(error.message, /API unavailable.*HTTP 503/);
      assert.equal(error.message.includes(token), false);
      assert.equal(error.message.includes("raw upstream"), false);
      return true;
    }
  );

  await assert.rejects(
    checkWorkerAccess({
      workerName: "slopless-server",
      accountID: values.CLOUDFLARE_ACCOUNT_ID,
      token,
      fetchImpl: async () => {
        throw new Error("socket details must stay private");
      }
    }),
    (error) => {
      assert.match(error.message, /network request failed/);
      assert.equal(error.message.includes(token), false);
      assert.equal(error.message.includes("socket details"), false);
      return true;
    }
  );
});
