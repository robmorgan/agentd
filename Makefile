.PHONY: build install test dev-run go-build go-test

CARGO_BIN ?= $(or $(CARGO_HOME),$(HOME)/.cargo)/bin

# The agent CLI is Rust; the agentd daemon is Go (see go/README.md, which
# needs Zig 0.16+ to build libghostty-vt). The CLI looks for agentd next to
# its own executable, or at $AGENTD_BIN.
build: go-build
	cargo build

dev-run: build
	AGENTD_BIN=$(CURDIR)/go/bin/agentd ./target/debug/agent $(ARGS)

install: go-build
	cargo install --path crates/agent-cli
	install -m 0755 go/bin/agentd $(CARGO_BIN)/agentd

test: go-test
	cargo test

go-build:
	$(MAKE) -C go build

go-test:
	$(MAKE) -C go test
