# syntax=docker/dockerfile:1
#
# Air Defenders — Docker image.
#
# Pure-Go static binary on Chainguard's distroless static base. The Zenoh
# publisher in zenoh.go talks to the router's REST plugin over plain HTTP
# (pure stdlib net/http) so there is no CGO, no glibc requirement, and no
# build-tag gating. Whether the publisher actually emits is decided at run
# time by the `ZENOH_PUBLISH` env var (see ops/ansible/airdefenders.yml
# and the per-site group_vars). When the env var is unset, the runtime
# image and Go binary are byte-identical to the pre-Zenoh shape.

# Build stage — Chainguard Go image (per docs/design/os-supply-chain-security.md)
FROM cgr.dev/chainguard/go:latest AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Copy ALL Go sources + the embedded HTML pages in one shot. The
# package is flat `package main`; main.go `//go:embed`s index.html
# (solo at GET /), mp.html (multiplayer at GET /mp), and demo.html
# (8-persona fabric demo at GET /demo).
COPY *.go index.html mp.html demo.html ./
# Pure-Go SQLite driver (modernc.org/sqlite) — no CGO ever.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/airdefenders .
# Pre-create /data owned by the runtime nonroot UID (65532). On first mount
# of a fresh named volume, Docker seeds the volume from this path and
# preserves ownership — so the SQLite open succeeds without host-side chown.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# Runtime stage — Chainguard distroless static, nonroot UID 65532 by default.
FROM cgr.dev/chainguard/static:latest
COPY --from=build /out/airdefenders /usr/local/bin/airdefenders
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8095
ENV DB_PATH=/data/scores.db
ENTRYPOINT ["/usr/local/bin/airdefenders"]
