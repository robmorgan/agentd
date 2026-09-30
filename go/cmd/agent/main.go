// Command agent is the client for agentd: it starts, lists, attaches to and
// manages coding-agent sessions on this machine or a remote one.
package main

import (
	"os"

	"github.com/robmorgan/agentd/go/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
