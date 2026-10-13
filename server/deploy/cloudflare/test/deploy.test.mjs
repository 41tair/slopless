import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
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
