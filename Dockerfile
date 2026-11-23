# Builds the Beacon image: a static binary on a small base, run as a non-root
# user. The web interface is built first and embedded in the binary. The Go
# and Node versions must match .tool-versions; CI checks that they do.

# The web interface. What it builds is the same for every platform, so the
# stage runs on the build machine's own architecture, once, and
# multi-platform builds need no emulation.
FROM --platform=$BUILDPLATFORM node:26.13.1-alpine3.24 AS web

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci

COPY web ./
# The build is written to where the Go code embeds it from.
RUN mkdir -p /src/internal/web/dist && npm run build

# The build stage runs on the build machine's own architecture and
# cross-compiles, so multi-platform builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24 AS build

ARG TARGETOS
ARG TARGETARCH
# Bare X.Y.Z for releases; set by the release workflow.
ARG VERSION=dev
# Commit the image was built from. The .git directory is not in the build
# context, so the Go toolchain cannot find it by itself.
ARG COMMIT=""

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/web/dist ./internal/web/dist
# An image without the web interface must not be built: the binary would
# work and the release would be broken.
RUN test -s internal/web/dist/index.html

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/thanoslabs/beacon/internal/version.Version=${VERSION} -X github.com/thanoslabs/beacon/internal/version.Commit=${COMMIT}" \
      -o /out/beacon ./cmd/beacon

FROM alpine:3.24.4

COPY --from=build /out/beacon /usr/local/bin/beacon

# A numeric user needs no entry in /etc/passwd, and lets Kubernetes verify
# runAsNonRoot.
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/beacon"]
CMD ["serve"]
