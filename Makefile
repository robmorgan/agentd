.PHONY: bootstrap-ghostty build install test dev-run

bootstrap-ghostty:
	./scripts/bootstrap-ghostty.sh

build:
	cargo build

dev-run: build
	./target/debug/agent $(ARGS)

install:
	cargo install --path crates/agentd
	cargo install --path crates/agent-cli

test:
	cargo test
