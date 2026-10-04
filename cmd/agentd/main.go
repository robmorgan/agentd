// Command agentd is the Go implementation of the agentd daemon and its
// session worker:
//
//	agentd serve [--daemonize]   run the daemon (the agent CLI starts it)
//	agentd upgrade               replace the running daemon, and hand running
//	                             sessions' workers, over to this binary
//	agentd bench sessions ...    measure what sessions cost, on a private daemon
//	agentd session-worker ...    one session's PTY owner (started by serve)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/internal/bench"
	"github.com/robmorgan/agentd/internal/config"
	"github.com/robmorgan/agentd/internal/daemon"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/transport"
	"github.com/robmorgan/agentd/internal/worker"
	"golang.org/x/term"
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
	case "bench":
		os.Exit(runBench(os.Args[2:]))
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
	fmt.Fprintln(os.Stderr, "       agentd bench sessions [--count N] [--output-heavy N] [--attach K] [--duration D] [--fill-lines L] [--json] [--json-file F]")
	fmt.Fprintln(os.Stderr, "       agentd session-worker --session-id ID --cwd DIR --created-at TS --session-uid UID --agent-name NAME --command CMD [--model M] [--arg A]...")
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

// runBench runs `agentd bench sessions`, which measures what sessions cost
// on a private daemon in a temporary root, never the user's.
func runBench(argv []string) int {
	if len(argv) == 0 || argv[0] != "sessions" {
		usage()
		return 2
	}
	fs := flag.NewFlagSet("bench sessions", flag.ContinueOnError)
	var opts bench.Options
	fs.IntVar(&opts.Count, "count", 10, "idle sessions to create")
	fs.IntVar(&opts.OutputHeavy, "output-heavy", 0, "sessions that write output continuously")
	fs.IntVar(&opts.Attach, "attach", 0, "clients to attach to one output-heavy session")
	fs.DurationVar(&opts.Duration, "duration", 5*time.Second, "how long to measure idle CPU, throughput and fan-out")
	fs.IntVar(&opts.FillLines, "fill-lines", 200_000, "lines printed by the full-scrollback session (0 skips it)")
	fs.IntVar(&opts.Parallel, "parallel", 8, "requests in flight while creating and sampling sessions")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	jsonFile := fs.String("json-file", "", "also write the report as JSON to this file")
	quiet := fs.Bool("quiet", false, "do not print progress")
	if err := fs.Parse(argv[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		usage()
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	opts.Exe = exe
	if !*quiet {
		opts.Log = os.Stderr
	}
	// Ctrl-C stops the run; Run then stops everything it started.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	report, err := bench.Run(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd bench: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd bench: %v\n", err)
		return 1
	}
	data = append(data, '\n')
	if *jsonFile != "" {
		if err := os.WriteFile(*jsonFile, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "agentd bench: %v\n", err)
			return 1
		}
	}
	if *asJSON {
		os.Stdout.Write(data)
		return 0
	}
	report.Print(os.Stdout)
	return 0
}

// runUpgrade replaces the daemon with this binary and hands every running
// session over to it. A session whose handoff fails keeps running on the
// binary it had, which is reported but does not fail the upgrade.
func runUpgrade() int {
	p, exe, err := discover()
	var results []daemon.HandoffResult
	if err == nil {
		results, err = daemon.Upgrade(p, exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		return 1
	}
	fmt.Println("✓ Upgraded daemon")
	for _, r := range results {
		switch {
		case r.HandedOff != nil:
			fmt.Printf("✓ Session %s now runs %s (live handoff, pid %d)\n", r.SessionID, r.HandedOff.Executable, r.HandedOff.WorkerPID)
		case r.Lost:
			fmt.Printf("✗ Session %s was lost during its handoff: %v\n", r.SessionID, r.Err)
		default:
			fmt.Printf("! Session %s keeps running on its previous binary: %v\n", r.SessionID, r.Err)
		}
	}
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
		case daemon.KindShared:
			fmt.Printf("No Tailscale address found. %s is in the shared carrier-grade NAT range: a mobile carrier,\n", candidate.IP)
			fmt.Println("ISP or VPN may have assigned it, and others on that network may reach the listener.")
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
	cfg, err := config.Load(p.Config)
	if err != nil {
		return fail(err)
	}
	previous := cfg.Remote.Listen
	if err := daemon.SetRemoteListen(p.Config, addr); err != nil {
		return fail(err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	// If the daemon does not come up listening on the new address, put the
	// previous setting back (and restart again, so a listener that worked
	// before keeps working) rather than leave a broken config behind.
	status, err = daemon.Restart(p, exe)
	if err == nil && status.Remote == "" {
		err = fmt.Errorf("the daemon is not listening on %s: %s", addr, status.RemoteError)
	}
	if err != nil {
		if rerr := daemon.SetRemoteListen(p.Config, previous); rerr != nil {
			return fail(fmt.Errorf("%v; restoring the previous setting also failed: %v", err, rerr))
		}
		if _, rerr := daemon.Restart(p, exe); rerr != nil {
			return fail(fmt.Errorf("%v; restarting with the previous setting also failed: %v", err, rerr))
		}
		return fail(fmt.Errorf("%v; the previous setting was restored", err))
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
	cfg, err := config.Load(p.Config)
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
	if status != nil {
		fmt.Printf("connected clients: %d\n", len(status.Connections))
		for _, c := range status.Connections {
			name := c.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("  %s %s from %s since %s: %d streams open, rtt %dus, %d/%d packets lost\n",
				name, c.Fingerprint, c.Remote, c.ConnectedAt.Local().Format(time.DateTime), c.StreamsOpen, c.RTTMicros, c.PacketsLost, c.PacketsSent)
		}
	}
	return 0
}

// confirm asks a yes/no question on the terminal and refuses without one:
// exposing the daemon on a network is never done by default.
func confirm(question string) (bool, error) {
	// isatty, not "is a character device": /dev/null is one too.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
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
	fs.StringVar(&args.UID, "session-uid", "", "UID of the session incarnation this worker is for")
	fs.StringVar(&args.AgentName, "agent-name", "", "agent name")
	fs.StringVar(&args.Command, "command", "", "agent command")
	fs.StringVar(&args.Model, "model", "", "model")
	fs.Var(&extra, "arg", "agent argument (repeatable)")
	probe := fs.Bool("handoff-probe", false, "print the handoff formats this binary can resume from")
	resumeFD := fs.Int("resume-fd", -1, "resume a session handed off by a previous image (internal)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	switch {
	case *probe:
		fmt.Println(worker.HandoffProbe())
		return 0
	case *resumeFD >= 0:
		if err := worker.Resume(*resumeFD); err != nil {
			fmt.Fprintf(os.Stderr, "session worker failed: %v\n", err)
			return 1
		}
		return 0
	}
	args.Args = extra
	for name, v := range map[string]string{
		"--session-id": args.SessionID, "--cwd": args.Cwd, "--created-at": args.CreatedAt, "--session-uid": args.UID,
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
