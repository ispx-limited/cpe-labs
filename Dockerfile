# syntax=docker/dockerfile:1.7
#
# cpe-sim, TR-069 / TR-369 CPE simulator.
#
# See docs/guides/quickstart.md for usage examples.

# ---- build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=docker
ARG COMMIT=unknown
ARG DATE
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    : "${DATE:=$(date -u +%Y-%m-%dT%H:%M:%SZ)}"; \
    CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w \
            -X github.com/ispx-limited/cpe-labs/internal/version.Version=${VERSION} \
            -X github.com/ispx-limited/cpe-labs/internal/version.Commit=${COMMIT} \
            -X github.com/ispx-limited/cpe-labs/internal/version.Date=${DATE}" \
        -o /out/cpe-sim ./cmd/cpe-sim

# ---- runtime stage ----
# Distroless static: no shell, no package manager, just the binary +
# CA bundle for outbound HTTPS to TLS-protected ACSes.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cpe-sim /usr/local/bin/cpe-sim
COPY profiles/ /profiles/
ENTRYPOINT ["/usr/local/bin/cpe-sim"]
