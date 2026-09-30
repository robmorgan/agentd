.PHONY: libghostty build agent agentd test vet

ZIG ?= zig
VERSION ?= 0.1.0
PKG_CONFIG_PATH := $(CURDIR)/.build/ghostty-out/share/pkgconfig
export PKG_CONFIG_PATH
# Both binaries report the same version.
LDFLAGS := -X github.com/robmorgan/agentd/go/internal/cli.Version=$(VERSION) \
	-X github.com/robmorgan/agentd/go/internal/daemon.Version=$(VERSION)

libghostty:
	ZIG=$(ZIG) ./scripts/build-libghostty.sh

build: agentd agent

# agentd links libghostty-vt (cgo, built with Zig).
agentd: libghostty
	go build -ldflags '$(LDFLAGS)' -o bin/agentd ./cmd/agentd

# agent is pure Go: it needs neither cgo nor Zig.
agent:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/agent ./cmd/agent

test: libghostty
	go test -race ./...

vet: libghostty
	go vet ./...
