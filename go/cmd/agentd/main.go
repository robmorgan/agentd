// Command agentd is the Go implementation of the agentd daemon and its
// session worker:
//
//	agentd serve [--daemonize]   run the daemon (the agent CLI starts it)
//	agentd upgrade               replace a running daemon with this binary
//	agentd session-worker ...    one session's PTY owner (started by serve)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/robmorgan/agentd/go/internal/daemon"
	"github.com/robmorgan/agentd/go/internal/paths"
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
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "upgrade":
		os.Exit(runUpgrade())
	case "session-worker":
		os.Exit(runSessionWorker(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentd serve [--daemonize]")
	fmt.Fprintln(os.Stderr, "       agentd upgrade")
	fmt.Fprintln(os.Stderr, "       agentd session-worker --session-id ID --cwd DIR --created-at TS --agent-name NAME --command CMD [--model M] [--arg A]...")
}

func runServe(argv []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	detach := fs.Bool("daemonize", false, "start the daemon in the background and return")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	p, exe, err := discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	if *detach {
		if err := daemon.Daemonize(p, exe); err != nil {
			fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
			return 1
		}
		return 0
	}
	srv, err := daemon.New(p, exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	// SIGTERM/SIGINT stop the daemon; sessions keep running in their
	// workers and are picked up again by the next daemon.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	return 0
}

func runUpgrade() int {
	p, exe, err := discover()
	if err == nil {
		err = daemon.Upgrade(p, exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	fmt.Println("✓ Upgraded daemon")
	return 0
}

func discover() (*paths.AppPaths, string, error) {
	p, err := paths.Discover()
	if err != nil {
		return nil, "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve agentd executable: %w", err)
	}
	return p, exe, nil
}

func runSessionWorker(argv []string) int {
	fs := flag.NewFlagSet("session-worker", flag.ContinueOnError)
	var args worker.Args
	var extra multiFlag
	fs.StringVar(&args.SessionID, "session-id", "", "session id")
	fs.StringVar(&args.Cwd, "cwd", "", "working directory for the agent process")
	fs.StringVar(&args.CreatedAt, "created-at", "", "creation timestamp of the session row this worker is for")
	fs.StringVar(&args.AgentName, "agent-name", "", "agent name")
	fs.StringVar(&args.Command, "command", "", "agent command")
	fs.StringVar(&args.Model, "model", "", "model")
	fs.Var(&extra, "arg", "agent argument (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	args.Args = extra
	for name, v := range map[string]string{
		"--session-id": args.SessionID, "--cwd": args.Cwd, "--created-at": args.CreatedAt,
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
