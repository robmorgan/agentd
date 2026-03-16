.PHONY: bootstrap-ghostty build install test

bootstrap-ghostty:
	./scripts/bootstrap-ghostty.sh

build:
	cargo build

install:
	cargo install --path crates/agentd
	cargo install --path crates/agent

test:
	cargo test
