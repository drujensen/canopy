# Optional, secondary packaging path (Plan Phase 7): the primary way to run
# Canopy is the standalone static binary built by `make build` (see
# README.md's "Build and run" section) — this Dockerfile exists for
# deployments that specifically want a container, not because Canopy needs
# one.
#
# Build:  docker build --build-arg VERSION=v0.1.0 -t canopy:v0.1.0 .
# Run:    docker run --rm -it -v "$HOME/.canopy:/root/.canopy" -v "$PWD:/workspace" -w /workspace canopy:v0.1.0

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X 'main.version=${VERSION}'" \
    -o /out/canopy ./cmd/canopy

# Distroless static base: no shell, no package manager, just the binary and
# CA certs (needed for TLS to provider APIs) — matches the "single static
# binary, no runtime dependency beyond the OS" claim this image is meant to
# preserve rather than undercut with a full Linux userland.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/canopy /usr/local/bin/canopy
ENTRYPOINT ["/usr/local/bin/canopy"]
