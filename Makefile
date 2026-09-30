.PHONY: libghostty build agent agentd test vet dev-run install

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

# The CLI looks for agentd next to its own executable, or at $AGENTD_BIN.
dev-run: build
	AGENTD_BIN=$(CURDIR)/bin/agentd ./bin/agent $(ARGS)

install: build
	install -d $(BINDIR)
	install -m 0755 bin/agent bin/agentd $(BINDIR)/
	@if [ -x "$(HOME)/.cargo/bin/agent" ]; then \
		echo "note: $(HOME)/.cargo/bin/agent is the old Rust CLI; remove it (cargo uninstall agent-cli) so $(BINDIR)/agent is found"; \
	fi
