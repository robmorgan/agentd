.PHONY: build install test dev-run

BINDIR ?= $(or $(GOBIN),$(shell go env GOPATH)/bin)

# Both binaries are built in go/ (see go/README.md; agentd needs Zig 0.16+
# for libghostty-vt). The CLI looks for agentd next to its own executable,
# or at $AGENTD_BIN.
build:
	$(MAKE) -C go build

dev-run: build
	AGENTD_BIN=$(CURDIR)/go/bin/agentd ./go/bin/agent $(ARGS)

install: build
	install -d $(BINDIR)
	install -m 0755 go/bin/agent go/bin/agentd $(BINDIR)/
	@if [ -x "$(HOME)/.cargo/bin/agent" ]; then \
		echo "note: $(HOME)/.cargo/bin/agent is the old Rust CLI; remove it (cargo uninstall agent-cli) so $(BINDIR)/agent is found"; \
	fi

test:
	$(MAKE) -C go test
