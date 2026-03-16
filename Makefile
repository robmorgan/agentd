.PHONY: build install test

build:
	cargo build

install:
	cargo install --path crates/agentd
	cargo install --path crates/agent

test:
	cargo test
