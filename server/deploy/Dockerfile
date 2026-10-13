FROM golang:1.26.2-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/slopless ./cmd

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 slopless \
    && adduser -S -D -H -u 10001 -G slopless slopless

COPY --from=build --chown=10001:10001 /out/slopless /usr/local/bin/slopless

USER 10001:10001
EXPOSE 8090
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/slopless", "-config", "/etc/slopless/config.yaml"]
