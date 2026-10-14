#!/usr/bin/env node

import { randomUUID } from "node:crypto";
import { readFile, unlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { spawn } from "node:child_process";

const directory = dirname(fileURLToPath(import.meta.url));
const templatePath = join(directory, "wrangler.template.json");
const wranglerPath = join(directory, "node_modules", ".bin", "wrangler");
const workerNotFoundCodes = new Set([10007, 10090]);
const authorizationErrorCodes = new Set([10000, 9106]);
const workerAccessTimeoutMilliseconds = 15_000;

function valueAfter(args, index, option) {
  const value = args[index + 1];
  if (!value || value.startsWith("--")) throw new Error(`${option} requires a value`);
  return value;
}

export function parseArguments(args, environment = {}) {
  const options = {
    image: environment.IMAGE,
    commit: environment.COMMIT ?? environment.GITHUB_SHA,
    accountID: environment.CLOUDFLARE_ACCOUNT_ID,
    databaseID: environment.CLOUDFLARE_D1_DATABASE_ID ?? environment.D1_DATABASE_ID,
    repository: environment.CLOUDFLARE_CONTAINER_REPOSITORY ?? "slopless-server",
    sleepAfter: environment.CONTAINER_SLEEP_AFTER ?? "10m",
    dryRun: false,
    help: false
  };

  for (let index = 0; index < args.length; index += 1) {
    const option = args[index];
    if (option === "--dry-run") options.dryRun = true;
    else if (option === "--help" || option === "-h") options.help = true;
    else if (option === "--image") options.image = valueAfter(args, index++, option);
    else if (option === "--commit") options.commit = valueAfter(args, index++, option);
    else if (option === "--account-id") options.accountID = valueAfter(args, index++, option);
    else if (option === "--database-id") options.databaseID = valueAfter(args, index++, option);
    else if (option === "--repository") options.repository = valueAfter(args, index++, option);
    else if (option === "--sleep-after") options.sleepAfter = valueAfter(args, index++, option);
    else throw new Error(`unknown option: ${option}`);
  }

  if (options.help) return options;
  for (const [name, value] of Object.entries({
    IMAGE: options.image,
    COMMIT: options.commit,
    CLOUDFLARE_ACCOUNT_ID: options.accountID,
    CLOUDFLARE_D1_DATABASE_ID: options.databaseID
  })) {
    if (!value?.trim()) throw new Error(`${name} is required`);
  }

  options.image = options.image.trim().replace(/\/$/, "");
  options.commit = options.commit.trim().toLowerCase();
  options.accountID = options.accountID.trim().toLowerCase();
  options.databaseID = options.databaseID.trim().toLowerCase();
  options.repository = options.repository.trim();
  options.sleepAfter = options.sleepAfter.trim();

  if (options.image.includes("@") || /:[^/]+$/.test(options.image)) {
    throw new Error("IMAGE must be a repository without a tag or digest");
  }
  if (!/^[0-9a-f]{40}$/.test(options.commit)) throw new Error("COMMIT must be a full 40-character Git SHA");
  if (!/^[0-9a-f]{32}$/.test(options.accountID)) throw new Error("CLOUDFLARE_ACCOUNT_ID must be a 32-character hexadecimal ID");
  if (!/^[0-9a-f-]{32,36}$/.test(options.databaseID)) throw new Error("CLOUDFLARE_D1_DATABASE_ID is invalid");
  if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(options.repository)) throw new Error("invalid Cloudflare container repository");
  if (!/^\d+(?:s|m|h|d)$/.test(options.sleepAfter)) throw new Error("CONTAINER_SLEEP_AFTER must look like 10m");

  return options;
}

export function deploymentPlan(options) {
  const tag = `sha-${options.commit}`;
  return {
    sourceImage: `${options.image}:${tag}`,
    localImage: `${options.repository}:${tag}`,
    deployedImage: `registry.cloudflare.com/${options.accountID}/${options.repository}:${tag}`
  };
}

export function renderConfig(template, options, plan = deploymentPlan(options)) {
  const replacements = new Map([
    ["__CLOUDFLARE_ACCOUNT_ID__", options.accountID],
    ["__D1_DATABASE_ID__", options.databaseID],
    ["__CONTAINER_IMAGE__", plan.deployedImage],
    ["__SLEEP_AFTER__", options.sleepAfter]
  ]);
  const replace = (value) => {
    if (typeof value === "string") return replacements.get(value) ?? value;
    if (Array.isArray(value)) return value.map(replace);
    if (value && typeof value === "object") {
      return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, replace(item)]));
    }
    return value;
  };
  const rendered = replace(JSON.parse(template));
  const encoded = JSON.stringify(rendered, null, 2) + "\n";
  if (/__[A-Z0-9_]+__/.test(encoded)) throw new Error("unresolved deployment template placeholder");
  return encoded;
}

function workerAccessError(workerName, accountID) {
  return new Error(
    `CLOUDFLARE_API_TOKEN cannot access Worker "${workerName}" in target account ${accountID}; ` +
    "grant Workers Scripts Write (or Workers Editor), and use Workers Admin at the Workers product scope for the first deployment"
  );
}

export async function checkWorkerAccess({
  workerName,
  accountID,
  token,
  fetchImpl = globalThis.fetch
}) {
  const endpoint =
    `https://api.cloudflare.com/client/v4/accounts/${encodeURIComponent(accountID)}` +
    `/workers/scripts/${encodeURIComponent(workerName)}/secrets`;

  let response;
  try {
    response = await fetchImpl(endpoint, {
      method: "GET",
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(workerAccessTimeoutMilliseconds)
    });
  } catch (error) {
    if (error?.name === "AbortError" || error?.name === "TimeoutError") {
      throw new Error("Cloudflare Worker access preflight timed out after 15 seconds");
    }
    throw new Error("Cloudflare Worker access preflight network request failed");
  }

  let payload;
  try {
    payload = await response.json();
  } catch {
    if (response.status >= 500) {
      throw new Error(`Cloudflare API unavailable during Worker access preflight (HTTP ${response.status})`);
    }
    throw new Error(`Cloudflare Worker access preflight received an invalid API response (HTTP ${response.status})`);
  }

  const errorCodes = Array.isArray(payload?.errors)
    ? payload.errors.map((error) => Number(error?.code)).filter(Number.isFinite)
    : [];
  const authorizationDenied =
    response.status === 401 ||
    response.status === 403 ||
    errorCodes.some((code) => authorizationErrorCodes.has(code));
  if (authorizationDenied) throw workerAccessError(workerName, accountID);

  if (response.status >= 200 && response.status < 300 && payload?.success === true) return;

  const onlyWorkerNotFound =
    errorCodes.length > 0 && errorCodes.every((code) => workerNotFoundCodes.has(code));
  if (response.status >= 400 && response.status < 500 && onlyWorkerNotFound) return;

  if (response.status >= 500) {
    throw new Error(`Cloudflare API unavailable during Worker access preflight (HTTP ${response.status})`);
  }
  throw new Error(`Cloudflare Worker access preflight failed (HTTP ${response.status})`);
}

function run(command, args, environment) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: directory,
      env: environment,
      stdio: "inherit"
    });
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (code === 0) resolve();
      else reject(new Error(`${command} failed${signal ? ` with signal ${signal}` : ` with exit code ${code}`}`));
    });
  });
}

export async function deploy(options, environment = process.env) {
  const plan = deploymentPlan(options);
  const template = await readFile(templatePath, "utf8");
  const config = renderConfig(template, options, plan);

  if (options.dryRun) {
    return { ...plan, config: JSON.parse(config) };
  }
  if (!environment.CLOUDFLARE_API_TOKEN?.trim()) {
    throw new Error("CLOUDFLARE_API_TOKEN is required for deployment");
  }

  const renderedConfig = JSON.parse(config);
  await checkWorkerAccess({
    workerName: renderedConfig.name,
    accountID: options.accountID,
    token: environment.CLOUDFLARE_API_TOKEN.trim()
  });

  const generatedConfig = join(directory, `.wrangler.generated-${process.pid}-${randomUUID()}.json`);
  try {
    const dockerEnvironment = { ...environment };
    delete dockerEnvironment.CLOUDFLARE_API_TOKEN;
    await run("docker", ["tag", plan.sourceImage, plan.localImage], dockerEnvironment);
    await run(wranglerPath, ["containers", "push", plan.localImage], environment);
    await writeFile(generatedConfig, config, { encoding: "utf8", mode: 0o600, flag: "wx" });
    await run(wranglerPath, ["d1", "migrations", "apply", "DB", "--remote", "--config", generatedConfig], environment);
    await run(wranglerPath, ["deploy", "--config", generatedConfig], environment);
  } finally {
    await unlink(generatedConfig).catch((error) => {
      if (error.code !== "ENOENT") throw error;
    });
  }

  return plan;
}

function usage() {
  return `Usage: node deploy.mjs [options]

Options:
  --image REPOSITORY       Source image repository (or IMAGE)
  --commit SHA             Full Git commit (or COMMIT/GITHUB_SHA)
  --account-id ID          Cloudflare account ID (or CLOUDFLARE_ACCOUNT_ID)
  --database-id ID         D1 database ID (or CLOUDFLARE_D1_DATABASE_ID)
  --repository NAME        Cloudflare container repository (default: slopless-server)
  --sleep-after DURATION   Idle sleep duration (default: 10m)
  --dry-run                Validate and render without Docker or Cloudflare calls
`;
}

export async function main(args = process.argv.slice(2), environment = process.env, output = console) {
  const options = parseArguments(args, environment);
  if (options.help) {
    output.log(usage());
    return;
  }
  const plan = await deploy(options, environment);
  if (options.dryRun) {
    output.log(JSON.stringify({
      sourceImage: plan.sourceImage,
      localImage: plan.localImage,
      deployedImage: plan.deployedImage,
      worker: plan.config.name,
      dryRun: true
    }));
  } else {
    output.log(`Deployed ${plan.deployedImage}`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    console.error(`cloudflare deploy: ${error.message}`);
    process.exitCode = 1;
  });
}
