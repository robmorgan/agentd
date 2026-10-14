.PHONY: libghostty build agt agentd test test-cgocheck test-linux vet dev-run install

ZIG ?= zig
VERSION ?= 0.1.0
BINDIR ?= $(or $(GOBIN),$(shell go env GOPATH)/bin)
PKG_CONFIG_PATH := $(CURDIR)/.build/ghostty-out/share/pkgconfig
export PKG_CONFIG_PATH
# Both binaries report the same version.
LDFLAGS := -X github.com/robmorgan/agentd/internal/cli.Version=$(VERSION) \
	-X github.com/robmorgan/agentd/internal/daemon.Version=$(VERSION)

# libghostty-vt, which agentd links, needs Zig 0.16+ (see README.md).
libghostty:
	ZIG=$(ZIG) ./scripts/build-libghostty.sh

build: agentd agt

# agentd links libghostty-vt (cgo, built with Zig).
agentd: libghostty
	go build -ldflags '$(LDFLAGS)' -o bin/agentd ./cmd/agentd

# agt is pure Go: it needs neither cgo nor Zig.
agt:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/agt ./cmd/agt

test: libghostty
	go test -race ./...

# The worker's tests with cgo's full pointer checks, which catch Go pointers
# stored in C memory (libghostty's bindings, or our calls into them).
test-cgocheck: libghostty
	GOEXPERIMENT=cgocheck2 go test ./internal/worker/

# The same suite on Linux: directly on a Linux host, otherwise in Docker
# (scripts/test-linux.sh; AGENTD_TEST_DOCKER=1 forces Docker).
test-linux:
	./scripts/test-linux.sh

vet: libghostty
	go vet ./...

# The CLI looks for agentd next to its own executable, or at $AGENTD_BIN.
dev-run: build
	AGENTD_BIN=$(CURDIR)/bin/agentd ./bin/agt $(ARGS)

install: build
	install -d $(BINDIR)
	install -m 0755 bin/agt bin/agentd $(BINDIR)/
