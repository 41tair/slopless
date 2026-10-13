IMAGE ?= ghcr.io/41tair/slopless
TAG ?= latest
SOURCE ?= https://github.com/41tair/slopless

DOCKERFILE := server/deploy/Dockerfile
CONTEXT := server

.PHONY: docker-build docker-push

docker-build:
	docker build \
		--file "$(DOCKERFILE)" \
		--tag "$(IMAGE):$(TAG)" \
		--label "org.opencontainers.image.source=$(SOURCE)" \
		"$(CONTEXT)"

docker-push:
	docker push "$(IMAGE):$(TAG)"
