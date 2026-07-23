# syntax=docker/dockerfile:1.7

ARG ALPINE_VERSION=3.23
ARG NODE_VERSION=24
ARG GO_VERSION=1.26

FROM node:${NODE_VERSION}-alpine${ALPINE_VERSION} AS frontend-builder
WORKDIR /src/web
COPY web/package*.json ./
RUN --mount=type=cache,target=/root/.npm,sharing=locked npm ci
COPY web/ ./
RUN npm run build

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS backend-builder
ARG VERSION=unknown
ARG BUILDTIME=unknown
ARG REVISION=unknown
ARG ENABLE_UPX=0
ARG VOHIVE_MINISIGN_PUBLIC_KEYS=unconfigured
ENV GOTOOLCHAIN=auto
ENV GOWORK=off
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY --from=frontend-builder /src/web/dist ./internal/web/dist
RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod verify && \
    mkdir -p /out && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -tags "with_utls nomsgpack" \
      -ldflags "-s -w -X 'github.com/zanescope/vohive/internal/global.Version=${VERSION}' -X 'github.com/zanescope/vohive/internal/global.BuildTime=${BUILDTIME}' -X 'github.com/zanescope/vohive/internal/updater.UpdaterVersion=${VERSION}' -X 'github.com/zanescope/vohive/internal/updater.TrustedMinisignPublicKeys=${VOHIVE_MINISIGN_PUBLIC_KEYS}'" \
      -o /out/vohive ./cmd/vohive && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -tags "with_utls nomsgpack" \
      -ldflags "-s -w -X 'github.com/zanescope/vohive/internal/global.Version=${VERSION}' -X 'github.com/zanescope/vohive/internal/global.BuildTime=${BUILDTIME}' -X 'github.com/zanescope/vohive/internal/updater.UpdaterVersion=${VERSION}' -X 'github.com/zanescope/vohive/internal/updater.TrustedMinisignPublicKeys=${VOHIVE_MINISIGN_PUBLIC_KEYS}'" \
      -o /out/vohivectl ./cmd/vohivectl && \
    if [ "${ENABLE_UPX}" = "1" ] || [ "${ENABLE_UPX}" = "true" ]; then \
      (apk add --no-cache upx >/dev/null 2>&1 || apk add --no-cache upx-ucl >/dev/null 2>&1 || true); \
      if command -v upx >/dev/null 2>&1; then upx --best --lzma /out/vohive /out/vohivectl; fi; \
    fi

FROM alpine:${ALPINE_VERSION} AS runtime
ARG VERSION=unknown
ARG BUILDTIME=unknown
ARG REVISION=unknown
ARG SOURCE_URL=https://github.com/zanescope/vohive
LABEL org.opencontainers.image.title="VoHive" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILDTIME}" \
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0"
RUN apk add --no-cache ca-certificates tzdata && \
    mkdir -p /app/config /app/data /app/logs
COPY --from=backend-builder /out/vohive /usr/local/bin/vohive
COPY --from=backend-builder /out/vohivectl /usr/local/bin/vohivectl
WORKDIR /app
ENV CONFIG_PATH=/app/config/config.yaml
EXPOSE 7575
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/vohivectl", "probe", "--liveness", "--config", "/app/config/config.yaml"]
ENTRYPOINT ["/usr/local/bin/vohive"]
CMD ["-c", "/app/config/config.yaml"]
