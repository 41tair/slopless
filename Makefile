IMAGE ?= ghcr.io/41tair/slopless
COMMIT ?= $(shell git rev-parse HEAD)
SOURCE ?= https://github.com/41tair/slopless

COMMIT_TAG := sha-$(COMMIT)
DOCKERFILE := server/deploy/Dockerfile
CONTEXT := server
CF_DEPLOY_DIR := server/deploy/cloudflare

.PHONY: docker-build docker-push cf-deploy-deps cf-check cf-deploy

docker-build:
	docker build \
		--platform linux/amd64 \
		--file "$(DOCKERFILE)" \
		--tag "$(IMAGE):$(COMMIT_TAG)" \
		--tag "$(IMAGE):latest" \
		--label "org.opencontainers.image.source=$(SOURCE)" \
		--label "org.opencontainers.image.revision=$(COMMIT)" \
		"$(CONTEXT)"

docker-push:
	docker push "$(IMAGE):$(COMMIT_TAG)"
	docker push "$(IMAGE):latest"

cf-deploy-deps:
	npm ci --prefix "$(CF_DEPLOY_DIR)"

cf-check: cf-deploy-deps
	npm run check --prefix "$(CF_DEPLOY_DIR)"
	IMAGE="$(IMAGE)" COMMIT="$(COMMIT)" node "$(CF_DEPLOY_DIR)/deploy.mjs" --dry-run

# Build or pull IMAGE:sha-COMMIT before deployment. The release reuses that image.
cf-deploy: cf-deploy-deps
	IMAGE="$(IMAGE)" COMMIT="$(COMMIT)" node "$(CF_DEPLOY_DIR)/deploy.mjs"
