.PHONY: build install test dev-run ensure-ghostty ensure-ghostty-lib

ensure-ghostty:
	git submodule update --init --recursive vendor/ghostty

ensure-ghostty-lib: ensure-ghostty
	cd vendor/ghostty && zig build -Demit-lib-vt

build:
	cargo build

dev-run: build
	./target/debug/agent $(ARGS)

install: ensure-ghostty-lib
	cargo install --path crates/agentd
	cargo install --path crates/agent-cli

test:
	cargo test
