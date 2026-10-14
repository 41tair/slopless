# Cloudflare Containers deployment

This directory deploys the already-built server image to one Cloudflare Container instance. The Worker serves `https://slopless.byron.fun`, waits up to 60 seconds for port `8090`, and forwards requests to the fixed `main` instance. The container has no direct public ingress and has no general Internet access. `workers.dev` and version preview URLs are disabled.

The container sends D1 requests to `http://d1.internal`. `ServerContainer.outboundByHost` handles only `POST /query` and `POST /raw`, then invokes the Worker's native `DB` binding. This keeps application traffic off the Cloudflare account-management REST API and its global API-token rate limit. `/query` uses `D1Database.batch`; `/raw` permits one read-only statement and uses `raw({ columnNames: true })` so the Go client retains column order.

## One-time setup

Create a D1 database and a Cloudflare API token whose resource scope includes
the account selected by `CLOUDFLARE_ACCOUNT_ID`. For classic token permissions,
grant Workers Scripts, D1, and Containers write access (shown as Edit/Write in
the dashboard). See the [API token permission list](https://developers.cloudflare.com/fundamentals/api/reference/permissions/).

If using the newer granular roles, first-time deployment needs **Admin at the
Workers product scope** to create `slopless-server`; Editor or a scope limited
to an existing Worker is insufficient for creation. Once the Worker exists,
deployment can use Editor scoped to that Worker. Retain D1 and Containers
permissions for migrations and image publication. See [Workers authorization](https://developers.cloudflare.com/workers/authorization/).

Custom Domain publication also needs **Workers Routes Write** on the
`byron.fun` zone. The zone must be active in the same Cloudflare account.
The hostname is set in `wrangler.template.json`; it needs no additional GitHub
variable. Cloudflare manages its DNS record and HTTPS certificate. See
[Custom Domains](https://developers.cloudflare.com/workers/configuration/routing/custom-domains/)
and [Workers permissions](https://developers.cloudflare.com/workers/authorization/workers/).

Store the token manually as the GitHub Actions repository secret
`CLOUDFLARE_API_TOKEN`. Updating a token in Cloudflare does not replace a
different token already saved in GitHub. Configure these non-secret repository
variables:

- `CLOUDFLARE_ACCOUNT_ID`
- `CLOUDFLARE_D1_DATABASE_ID`

Do not put the API token in this directory, Wrangler configuration, the image, or GitHub variables. The deploy script reads it only from the process environment and never passes it on a command line.

The Worker replaces all client-supplied forwarding headers with `X-Forwarded-For` derived from Cloudflare's `CF-Connecting-IP`. Cloudflare does not publish a stable Worker-to-Container source CIDR, so the container trusts all proxy peer addresses. That setting is safe only while port `8090` remains reachable exclusively through this Worker. Do not expose the container port through another ingress without narrowing `SERVER_TRUSTED_PROXIES` or adding an authenticated proxy boundary.

## Deploy

From the repository root, build and deploy through Make:

```sh
make docker-build
make cf-deploy
```

`cf-deploy` installs the exact tool versions from the lockfile. It expects
`CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and
`CLOUDFLARE_D1_DATABASE_ID` in the environment. The Action builds and pushes both
`sha-<full commit>` and `latest` only after a push to `master`, then calls
`make cf-deploy` with that commit. Deployment always uses the commit tag.

To deploy an existing revision from another machine, pull its `linux/amd64`
image first and pass the full revision to Make:

```sh
docker pull --platform linux/amd64 ghcr.io/41tair/slopless:sha-<full-commit>
make cf-deploy COMMIT=<full-commit>
```

The source image must already exist locally as `IMAGE:sha-COMMIT`. The script
first checks read access to the target Worker's Secrets API, then retags the
image for Cloudflare, runs `wrangler containers push`, writes a short-lived
rendered Wrangler configuration, applies remote D1 migrations, and runs
`wrangler deploy`. The check catches denied Worker access before a migration;
it cannot prove permission to create or update a Worker. It does not rebuild
the image. Migrations must remain compatible with the currently deployed
Worker because they are applied before the new Worker version starts serving
traffic.

If deployment fails on `/workers/scripts/slopless-server/secrets` with
`No access to the specified resource`, check the token's Workers permissions
and resource scope, then replace the GitHub secret if you created a new token.
Wrangler queries that endpoint to detect variable/binding names that collide
with existing Worker secrets; no application Secret needs to be created there.
An applied migration remains applied. Rerunning deployment lets Wrangler skip
the already-recorded migration and continue with Worker deployment.

```sh
CLOUDFLARE_API_TOKEN=... \
CLOUDFLARE_ACCOUNT_ID=0123456789abcdef0123456789abcdef \
CLOUDFLARE_D1_DATABASE_ID=00000000-0000-0000-0000-000000000000 \
IMAGE=ghcr.io/41tair/slopless \
COMMIT="$(git rev-parse HEAD)" \
node deploy.mjs
```

Use the repository Makefile in CI and normal releases. `make cf-check` installs
the locked tools, runs protocol and Container registry tests, and validates the
deployment plan; it requires the account/database variables but no API token.
Run the script directly only while diagnosing the deploy wrapper. Its dry run
validates all inputs and renders the final configuration without requiring
`CLOUDFLARE_API_TOKEN`, Docker, network access, or Cloudflare access:

```sh
node deploy.mjs --dry-run \
  --image ghcr.io/41tair/slopless \
  --commit 0123456789abcdef0123456789abcdef01234567 \
  --account-id 0123456789abcdef0123456789abcdef \
  --database-id 00000000-0000-0000-0000-000000000000
```

`CONTAINER_SLEEP_AFTER` defaults to `10m`. The image continues to run as its non-root user and keeps its existing entrypoint.

## D1 query and commit behavior

Reads execute the requested SQL on D1. Business transactions read only the
records they need, keep their pending changes in Go, and submit those changed
rows in one D1 batch guarded by a revision check. They never copy the database
to local SQLite. Ent supplies query builders and scanning; its interactive SQL
transactions are unavailable on D1. A conflict discards the plan and reruns the
business callback. A transaction marker resolves a lost commit response.

Schema changes run during release. Container startup only checks the schema
version. Migration files must remain compatible with the previous running
version. Application-table writes performed through manual SQL must increment
`__slopless_meta.version` in the same atomic batch, or be performed while
application traffic is stopped. Unversioned writes invalidate the commit
protocol. `Driver.Batch` is reserved for migrations and tests.

Start with one instance. Native binding traffic avoids the management API's
global token quota, but unrelated writes still compete on one revision. Measure
conflict/retry rates and request latency before expanding; heavy concurrent
writers may need atomic domain SQL and narrower revision scopes. D1 query and
database limits still apply. Keep previous commit images for rollback, and
redeploy the old commit with Make rather than assuming a Worker-only rollback
also restores the container image.
