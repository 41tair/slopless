# Cloudflare Containers deployment

This directory deploys the already-built server image to one Cloudflare Container instance. The Worker owns the public route, waits up to 60 seconds for port `8090`, and forwards requests to the fixed `main` instance. The container has no direct public ingress and has no general Internet access.

The container sends D1 requests to `http://d1.internal`. `ServerContainer.outboundByHost` handles only `POST /query` and `POST /raw`, then invokes the Worker's native `DB` binding. This keeps application traffic off the Cloudflare account-management REST API and its global API-token rate limit. `/query` uses `D1Database.batch`; `/raw` permits one read-only statement and uses `raw({ columnNames: true })` so the Go client retains column order.

## One-time setup

Create a D1 database and an account-scoped Cloudflare API token with [`Workers Scripts Write`, `D1 Write`, and `Containers Write`](https://developers.cloudflare.com/fundamentals/api/reference/permissions/). Store the token manually as the GitHub Actions secret `CLOUDFLARE_API_TOKEN`. Configure these non-secret repository or environment variables:

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

The source image must already exist locally as `IMAGE:sha-COMMIT`. The script retags that image for Cloudflare, runs `wrangler containers push`, writes a short-lived rendered Wrangler configuration that refers to the immutable Cloudflare registry tag, applies remote D1 migrations, and runs `wrangler deploy`. It does not rebuild the image. Migrations must remain compatible with the currently deployed Worker because they are applied before the new Worker version starts serving traffic.

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
