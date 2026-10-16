.PHONY: libghostty build agt agtd test test-cgocheck test-linux vet shellcheck dev-run install

ZIG ?= zig
VERSION ?= 0.1.0
BINDIR ?= $(or $(GOBIN),$(shell go env GOPATH)/bin)
PKG_CONFIG_PATH := $(CURDIR)/.build/ghostty-out/share/pkgconfig
export PKG_CONFIG_PATH
# Both binaries report the same version.
LDFLAGS := -X github.com/robmorgan/agtd/internal/cli.Version=$(VERSION) \
	-X github.com/robmorgan/agtd/internal/daemon.Version=$(VERSION)

# libghostty-vt, which agtd links, needs Zig 0.16+ (see README.md).
libghostty:
	ZIG=$(ZIG) ./scripts/build-libghostty.sh

build: agtd agt

# agtd links libghostty-vt (cgo, built with Zig).
agtd: libghostty
	go build -ldflags '$(LDFLAGS)' -o bin/agtd ./cmd/agtd

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
# (scripts/test-linux.sh; AGTD_TEST_DOCKER=1 forces Docker).
test-linux:
	./scripts/test-linux.sh

vet: libghostty
	go vet ./...

# Every tracked shell script (CI runs this too: .github/workflows/shellcheck.yml).
shellcheck:
	git ls-files -z '*.sh' | xargs -0 shellcheck

# The CLI looks for agtd next to its own executable, or at $AGTD_BIN.
dev-run: build
	AGTD_BIN=$(CURDIR)/bin/agtd ./bin/agt $(ARGS)

install: build
	install -d $(BINDIR)
	install -m 0755 bin/agt bin/agtd $(BINDIR)/
