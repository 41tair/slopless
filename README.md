# Slopless

Slopless helps you filter low-quality, AI-generated noise on GitHub. Mark accounts and use shared community marks to highlight, dim, or hide their pull requests and issues.

## Quick start

1. Clone this repository. Open `chrome://extensions`, enable **Developer mode**, click **Load unpacked**, and select `extension/`.
2. Need a token? Email [byron@linux.com](mailto:byron@linux.com), and I'll set up an account for you.
3. Open the extension's **Settings**, paste your API token, and keep the default endpoint: `https://slopless.byron.fun/openapi/v1`. Choose **Mark only**, **Dim**, or **Hide**, select pull requests and/or issues, then save.
4. Visit a GitHub profile, pull request, or issue. Click the Slopless icon to **Mark account** or **Unmark account**. The extension filters marked authors on repository pull request and issue lists.

## How it works

The extension reads GitHub authors and queries a Go API for shared account marks. Each user contributes one mark per account. Cloudflare D1 stores the data, and a Worker forwards requests to the Go service running in a Cloudflare Container.

## OpenAPI

The [OpenAPI specification](docs/openapi.json) documents authentication, account queries, and marking.

## Self-host

The [deployment guide](server/deploy/cloudflare/README.md) covers Docker, Cloudflare Containers, D1 migrations, Makefile commands, and GitHub Actions. Deploy your own service, then change the extension's endpoint to `https://your-domain/openapi/v1`.
