# Builds the Beacon image: a static binary on a small base, run as a non-root
# user. The Go version must match .tool-versions; CI checks that it does.

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
