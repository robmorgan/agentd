// Command agentd is the Go implementation of the agentd daemon and its
// session worker. During the port only the session-worker subcommand is
// implemented. Since protocol v33 the worker no longer speaks the Rust
// daemon's wire format, so it is driven by the Go daemon (in progress) and by
// the tests under internal/worker.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/robmorgan/agentd/go/internal/worker"
)

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "session-worker":
		os.Exit(runSessionWorker(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentd session-worker --session-id ID --cwd DIR --agent-name NAME --command CMD [--model M] [--arg A]...")
}

func runSessionWorker(argv []string) int {
	fs := flag.NewFlagSet("session-worker", flag.ContinueOnError)
	var args worker.Args
	var extra multiFlag
	fs.StringVar(&args.SessionID, "session-id", "", "session id")
	fs.StringVar(&args.Cwd, "cwd", "", "working directory for the agent process")
	fs.StringVar(&args.AgentName, "agent-name", "", "agent name")
	fs.StringVar(&args.Command, "command", "", "agent command")
	fs.StringVar(&args.Model, "model", "", "model")
	fs.Var(&extra, "arg", "agent argument (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	args.Args = extra
	for name, v := range map[string]string{
		"--session-id": args.SessionID, "--cwd": args.Cwd,
		"--agent-name": args.AgentName, "--command": args.Command,
	} {
		if v == "" {
			fmt.Fprintf(os.Stderr, "session-worker: %s is required\n", name)
			return 2
		}
	}
	if err := worker.Run(args); err != nil {
		fmt.Fprintf(os.Stderr, "session worker failed: %v\n", err)
		return 1
	}
	return 0
}
