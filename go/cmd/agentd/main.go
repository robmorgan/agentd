// Command agentd is the Go implementation of the agentd daemon and its
// session worker:
//
//	agentd serve [--daemonize]   run the daemon (the agent CLI starts it)
//	agentd upgrade               replace a running daemon with this binary
//	agentd session-worker ...    one session's PTY owner (started by serve)
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/robmorgan/agentd/go/internal/daemon"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/transport"
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
	case "remote":
		os.Exit(runRemote(os.Args[2:]))
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
	fmt.Fprintln(os.Stderr, "       agentd remote enable [ADDRESS] [--yes] | disable | status")
	fmt.Fprintln(os.Stderr, "       agentd remote id | list | authorize FINGERPRINT [NAME] | revoke FINGERPRINT")
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

// runRemote manages remote access: switching it on and off ([remote] listen
// in config.toml), the daemon's own key, and the client keys allowed to
// connect.
func runRemote(argv []string) int {
	p, err := paths.Discover()
	if err == nil {
		err = p.EnsureLayout()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	authorized := p.AuthorizedClientsPath()
	switch {
	case len(argv) >= 1 && argv[0] == "enable":
		return runRemoteEnable(p, argv[1:])
	case len(argv) == 1 && argv[0] == "disable":
		return runRemoteDisable(p)
	case len(argv) == 1 && argv[0] == "status":
		return runRemoteStatus(p)
	case len(argv) == 1 && argv[0] == "id":
		id, err := transport.LoadOrCreateIdentity(p.RemoteKeyPath())
		if err != nil {
			return fail(err)
		}
		fmt.Println(id.Fingerprint)
	case len(argv) == 1 && argv[0] == "list":
		clients, err := transport.ReadAuthorized(authorized)
		if err != nil {
			return fail(err)
		}
		for _, c := range clients {
			fmt.Println(strings.TrimSpace(c.Fingerprint + " " + c.Name))
		}
	case len(argv) >= 2 && argv[0] == "authorize":
		added, err := transport.Authorize(authorized, argv[1], strings.Join(argv[2:], " "))
		if err != nil {
			return fail(err)
		}
		if added {
			fmt.Printf("authorized %s\n", argv[1])
		} else {
			fmt.Printf("%s was already authorized\n", argv[1])
		}
	case len(argv) == 2 && argv[0] == "revoke":
		removed, err := transport.Revoke(authorized, argv[1])
		if err != nil {
			return fail(err)
		}
		if !removed {
			return fail(fmt.Errorf("%s is not authorized", argv[1]))
		}
		fmt.Printf("revoked %s\n", argv[1])
	default:
		usage()
		return 2
	}
	return 0
}

// runRemoteEnable sets [remote] listen and restarts the daemon to apply it.
// Without an address it uses this machine's Tailscale address, or asks
// before using the default-route address, which may be a LAN address that
// changes or a public one.
func runRemoteEnable(p *paths.AppPaths, argv []string) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	var given string
	yes := false
	for _, arg := range argv {
		switch {
		case arg == "--yes" || arg == "-y":
			yes = true
		case strings.HasPrefix(arg, "-") || given != "":
			usage()
			return 2
		default:
			given = arg
		}
	}

	var addr string
	if given != "" {
		var err error
		if addr, err = daemon.ListenAddress(given); err != nil {
			return fail(err)
		}
		if host, _, _ := net.SplitHostPort(addr); host == "0.0.0.0" || host == "::" {
			fmt.Println("Listening on every interface, including any public one. Only authorized keys can connect.")
		}
	} else {
		candidate, err := daemon.DetectListenAddress()
		if err != nil {
			return fail(fmt.Errorf("could not find an address to listen on (%v); pass one: agentd remote enable ADDRESS", err))
		}
		addr = net.JoinHostPort(candidate.IP.String(), daemon.DefaultRemotePort)
		switch candidate.Kind {
		case daemon.KindTailscale:
			fmt.Printf("Using this machine's Tailscale address, %s.\n", candidate.IP)
		case daemon.KindPrivate:
			fmt.Printf("No Tailscale address found. %s is a private LAN address: only machines on this network\n", candidate.IP)
			fmt.Println("can reach it, and it may change if it was assigned by DHCP.")
		default:
			fmt.Printf("No Tailscale address found. %s is a PUBLIC address: anyone on the internet can reach the\n", candidate.IP)
			fmt.Println("listener. Only authorized keys can connect, but a Tailscale or WireGuard address is safer.")
		}
		if candidate.Kind != daemon.KindTailscale && !yes {
			ok, err := confirm(fmt.Sprintf("Listen on %s?", addr))
			if err != nil {
				return fail(err)
			}
			if !ok {
				fmt.Println("Remote access not changed. To choose an address: agentd remote enable ADDRESS")
				return 1
			}
		}
	}

	status, err := daemon.Status(p)
	if err != nil {
		return fail(err)
	}
	current := ""
	if status != nil {
		current = status.Remote
	}
	if err := daemon.CheckListen(addr, current); err != nil {
		return fail(err)
	}
	if err := daemon.SetRemoteListen(p.Config, addr); err != nil {
		return fail(err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if status, err = daemon.Restart(p, exe); err != nil {
		return fail(err)
	}
	if status.Remote == "" {
		return fail(fmt.Errorf("saved [remote] listen = %q, but the daemon is not listening: %s", addr, status.RemoteError))
	}
	id, err := transport.LoadOrCreateIdentity(p.RemoteKeyPath())
	if err != nil {
		return fail(err)
	}
	fmt.Printf("✓ Accepting remote clients over QUIC on %s\n", status.Remote)
	fmt.Printf("  key %s\n\n", id.Fingerprint)
	fmt.Println("On the machine you connect from, run:")
	fmt.Printf("  agent host add %s %s --fingerprint %s\n", shortHostname(), status.Remote, id.Fingerprint)
	fmt.Println("and then authorize it here with the command it prints.")
	return 0
}

func runRemoteDisable(p *paths.AppPaths) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	if err := daemon.SetRemoteListen(p.Config, ""); err != nil {
		return fail(err)
	}
	status, err := daemon.Status(p)
	if err != nil {
		return fail(err)
	}
	if status != nil && (status.Remote != "" || status.RemoteError != "") {
		exe, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		if _, err := daemon.Restart(p, exe); err != nil {
			return fail(err)
		}
	}
	fmt.Println("✓ Remote access is off")
	return 0
}

func runRemoteStatus(p *paths.AppPaths) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	cfg, err := daemon.LoadConfig(p.Config)
	if err != nil {
		return fail(err)
	}
	status, err := daemon.Status(p)
	if err != nil {
		return fail(err)
	}
	switch {
	case cfg.Remote.Listen == "":
		fmt.Println("remote access: off (turn it on with `agentd remote enable`)")
	case status == nil:
		fmt.Printf("remote access: configured on %s; agentd is not running\n", cfg.Remote.Listen)
	case status.Remote != "":
		fmt.Printf("remote access: listening on %s\n", status.Remote)
	case status.RemoteError != "":
		fmt.Printf("remote access: configured on %s but not listening: %s\n", cfg.Remote.Listen, status.RemoteError)
	default:
		fmt.Printf("remote access: configured on %s; restart agentd to apply it\n", cfg.Remote.Listen)
	}
	id, err := transport.LoadOrCreateIdentity(p.RemoteKeyPath())
	if err != nil {
		return fail(err)
	}
	clients, err := transport.ReadAuthorized(p.AuthorizedClientsPath())
	if err != nil {
		return fail(err)
	}
	fmt.Printf("key: %s\n", id.Fingerprint)
	fmt.Printf("authorized clients: %d\n", len(clients))
	return 0
}

// confirm asks a yes/no question on the terminal and refuses without one:
// exposing the daemon on a network is never done by default.
func confirm(question string) (bool, error) {
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("no terminal to confirm on; pass an ADDRESS or --yes")
	}
	fmt.Printf("%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(answer) {
	case "y", "Y", "yes", "Yes":
		return true, nil
	}
	return false, nil
}

// shortHostname is this machine's name without its domain, as a suggested
// host name for `agent host add`.
func shortHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "devbox"
	}
	name, _, _ = strings.Cut(strings.ToLower(name), ".")
	// Host names follow session-name rules: a-z, 0-9 and single hyphens.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	if clean := strings.Trim(b.String(), "-"); clean != "" {
		return clean
	}
	return "devbox"
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
