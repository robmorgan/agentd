.PHONY: libghostty build test vet

ZIG ?= zig

libghostty:
	ZIG=$(ZIG) ./scripts/build-libghostty.sh

build: libghostty
	PKG_CONFIG_PATH=$(CURDIR)/.build/ghostty-out/share/pkgconfig go build -o bin/agentd ./cmd/agentd

test: libghostty
	PKG_CONFIG_PATH=$(CURDIR)/.build/ghostty-out/share/pkgconfig go test ./...

vet: libghostty
	PKG_CONFIG_PATH=$(CURDIR)/.build/ghostty-out/share/pkgconfig go vet ./...
