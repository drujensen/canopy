# Canopy build/release Makefile (Plan Phase 7).
#
# The primary distribution mechanism is a single static Go binary — see
# README.md's "Build and run" section. A Dockerfile exists too, but it's a
# secondary/optional path, not the primary one (Plan Phase 7).

MODULE     := github.com/drujensen/canopy
BINARY     := canopy
CMD        := ./cmd/canopy
DIST       := dist

# VERSION defaults to the current git describe (falls back to "dev" outside
# a git checkout, e.g. a source tarball); override explicitly for a real
# release: `make build VERSION=v0.1.0`.
VERSION    ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X 'main.version=$(VERSION)'

# Platforms `make build-all` cross-compiles for (Requirements §7
# "Portability" / Plan Phase 7's cross-compilation guidance).
PLATFORMS  := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build build-all clean fmt vet tidy test lint qa install docker

## build: static binary for the host GOOS/GOARCH, version baked in via -ldflags.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY) $(CMD)

## build-all: cross-compile a static binary for every platform in $(PLATFORMS).
# Each binary is named canopy_<version>_<os>_<arch>[.exe]; CGO is disabled so
# every target stays a static binary regardless of host toolchain (Go's own
# cross-compilation support makes this cheap: no C toolchain per target).
build-all:
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out=$(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch}$$ext; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(CMD) || exit 1; \
	done

## install: build and install into $GOBIN (or $GOPATH/bin), version baked in.
install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

## docker: build the optional Docker image (secondary packaging path).
docker:
	docker build --build-arg VERSION=$(VERSION) -t canopy:$(VERSION) .

## fmt/vet/tidy/test/qa: the AGENTS.md QA workflow, run in order by `make qa`.
fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

test:
	go test ./... -race

## qa: the AGENTS.md QA workflow, in order: fmt, vet, tidy, build, test -race.
qa: fmt vet tidy
	go build ./...
	go test ./... -race

clean:
	rm -rf $(DIST)
