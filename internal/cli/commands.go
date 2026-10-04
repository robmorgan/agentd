package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robmorgan/agentd/internal/config"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

func annotate(cmd *cobra.Command, usage, args string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["usage"] = usage
	cmd.Annotations["args"] = args
	return cmd
}

func withValue(cmd *cobra.Command, flag, value string) {
	cmd.Flags().Lookup(flag).Annotations = map[string][]string{"value": {value}}
}

func (a *app) commands() []*cobra.Command {
	var cmds []*cobra.Command

	runtime := a.command("runtime [SESSION]", "", optionalArg, func(args []string) error {
		id := firstArg(args)
		if _, err := a.connect([]*string{&id}, "", nil); err != nil {
			return err
		}
		return a.runRuntime(id)
	})
	runtime.Hidden = true
	cmds = append(cmds, runtime)

	var cwd, agent, workspace string
	newCmd := a.command("new [NAME]", "Start and attach to a new session", optionalArg, func(args []string) error {
		name := firstArg(args)
		if a.host == autoHostName {
			if err := a.placeSession(agent, workspace, cwd); err != nil {
				return err
			}
		}
		var req *protocol.CreateSession
		c, err := a.connect([]*string{&name}, "", func(c *client) (err error) {
			// Checked before a daemon may be started, so a bad --cwd or
			// name fails fast with its own error.
			req, err = c.newSessionRequest(name, cwd, agent, workspace)
			return err
		})
		if err != nil {
			return err
		}
		resp, err := c.call(&protocol.Request{CreateSession: req}, 0, func(r *protocol.Response) bool { return r.CreateSession != nil })
		if err != nil {
			return err
		}
		return c.attachSessionUID(resp.CreateSession.SessionID, resp.CreateSession.UID)
	})
	newCmd.Flags().StringVar(&cwd, "cwd", "", "Directory the agent runs in (default: the current directory, or the workspace root with --workspace, where DIR is relative to it)")
	newCmd.Flags().StringVar(&agent, "agent", "", "")
	newCmd.Flags().StringVar(&workspace, "workspace", "", "Run in a workspace (see `agent workspace`)")
	withValue(newCmd, "cwd", "DIR")
	withValue(newCmd, "agent", "AGENT")
	withValue(newCmd, "workspace", "NAME")
	cmds = append(cmds, annotate(newCmd, "[OPTIONS] [NAME]", "[NAME]"))

	var killRm bool
	kill := a.command("kill SESSION", "Stop a running session or remove its record", oneArg, func(args []string) error {
		return a.kill(args[0], killRm)
	})
	kill.Flags().BoolVar(&killRm, "rm", false, "")
	cmds = append(cmds, annotate(kill, "[OPTIONS] <SESSION_ID>", "<SESSION_ID>"))

	cmds = append(cmds, annotate(a.command("rm SESSION", "Stop and remove a session", oneArg, func(args []string) error {
		return a.kill(args[0], true)
	}), "<SESSION_ID>", "<SESSION_ID>"))

	cmds = append(cmds, annotate(a.command("attach SESSION", "Attach to a live session PTY", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		return c.attachSession(id)
	}), "<SESSION_ID>", "<SESSION_ID>"))

	var detachAttach string
	var detachAll bool
	detach := a.command("detach [SESSION]", "Detach one or more attached clients", optionalArg, func(args []string) error {
		id := firstArg(args)
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		id, err = resolveDetachSessionID(id)
		if err != nil {
			return err
		}
		var req *protocol.Request
		switch {
		case detachAll && detachAttach != "":
			return errors.New("use either `--all` or `--attach <attach_id>`, not both")
		case detachAll:
			req = &protocol.Request{DetachSession: &protocol.DetachSession{SessionID: id, All: true}}
		case detachAttach != "":
			req = &protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: id, AttachID: detachAttach}}
		default:
			return errors.New("shared attach requires either `--all` or `--attach <attach_id>`; use Ctrl-\\ to detach the local client")
		}
		if _, err := c.call(req, 0, func(r *protocol.Response) bool { return r.Ok != nil }); err != nil {
			return err
		}
		fmt.Printf("detached from %s\n", id)
		return nil
	})
	detach.Flags().StringVar(&detachAttach, "attach", "", "")
	detach.Flags().BoolVar(&detachAll, "all", false, "")
	withValue(detach, "attach", "ATTACH")
	cmds = append(cmds, annotate(detach, "[OPTIONS] [SESSION_ID]", "[SESSION_ID]"))

	var sendInput *cobra.Command
	sendInput = a.command("send-input SESSION [--source-session-id ID] DATA...", "Send background input to a live session", nil, func(args []string) error {
		in, err := parseSendInput(args)
		if errors.Is(err, errHelp) {
			writeHelp(os.Stdout, sendInput)
			return nil
		}
		if err != nil {
			return err
		}
		if in.host != "" {
			a.host = in.host
		}
		c, err := a.connect([]*string{&in.session}, "", nil)
		if err != nil {
			return err
		}
		req := &protocol.SendInput{SessionID: in.session, Data: []byte(strings.Join(in.data, " "))}
		if in.source != "" {
			req.SourceSessionID = &in.source
		}
		_, err = c.call(&protocol.Request{SendInput: req}, 0, func(r *protocol.Response) bool { return r.InputAccepted != nil })
		return err
	})
	// DATA takes everything after the session, flag-like words included, so
	// the command parses its own arguments.
	sendInput.DisableFlagParsing = true
	sendInput.Flags().String("source-session-id", "", "")
	withValue(sendInput, "source-session-id", "SOURCE_SESSION_ID")
	cmds = append(cmds, annotate(sendInput, "[OPTIONS] <SESSION_ID> <DATA>...", "<SESSION_ID>\n<DATA>..."))

	var vt bool
	history := a.command("history SESSION", "Print captured session history", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		resp, err := c.call(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: id, VT: vt}}, 0, func(r *protocol.Response) bool { return r.History != nil })
		if err != nil {
			return err
		}
		fmt.Print(resp.History.Data)
		return nil
	})
	history.Flags().BoolVar(&vt, "vt", false, "")
	cmds = append(cmds, annotate(history, "[OPTIONS] <SESSION_ID>", "<SESSION_ID>"))

	var listAll, listJSON bool
	list := visibleAlias(a.command("list", "List known sessions", noArgs, func([]string) error {
		if listAll {
			return a.listAllSessions(listJSON)
		}
		c, err := a.connect(nil, "", nil)
		if err != nil {
			return err
		}
		sessions, err := c.listSessions(0)
		if err != nil {
			return err
		}
		if listJSON {
			w, _ := c.welcome()
			listed := make([]hostSession, len(sessions))
			for i := range sessions {
				listed[i] = hostSession{host: c.remoteName(), welcome: w, rec: sessions[i]}
			}
			return writeSessionsJSON(os.Stdout, listed)
		}
		width := stdoutWidth()
		if width == 0 {
			width = sessionListDefaultWidth
		}
		for _, line := range renderSessionListLines(sessions, width, time.Now()) {
			fmt.Println(line)
		}
		return nil
	}), "ls", "sessions")
	list.Flags().BoolVarP(&listAll, "all", "a", false, "List the sessions of this machine and every host (see `agent hosts`)")
	list.Flags().BoolVar(&listJSON, "json", false, "Print one JSON object per session, with its host and global id")
	cmds = append(cmds, list)

	cmds = append(cmds, annotate(a.command("attachments SESSION", "Show currently attached clients for a session", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		resp, err := c.call(&protocol.Request{ListAttachments: &protocol.SessionRef{SessionID: id}}, 0, func(r *protocol.Response) bool { return r.Attachments != nil })
		if err != nil {
			return err
		}
		for _, at := range *resp.Attachments {
			fmt.Printf("%s\t%s\t%s\t%s\n", at.AttachID, at.SessionID, at.Kind, at.ConnectedAt.Format(time.RFC3339Nano))
		}
		return nil
	}), "<SESSION_ID>", "<SESSION_ID>"))

	var withStats bool
	status := a.command("status SESSION", "Show detailed session status", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		if withStats {
			if err := c.requireStats(); err != nil {
				return err
			}
		}
		resp, err := c.call(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}}, 0, func(r *protocol.Response) bool { return r.Session != nil })
		if err != nil {
			return err
		}
		if w, err := c.welcome(); err == nil {
			if host := c.remoteName(); host != "" {
				fmt.Printf("host: %s\n", host)
			}
			if gid := protocol.GlobalSessionID(w.DaemonID, resp.Session.UID); gid != "" {
				fmt.Printf("global_id: %s\n", gid)
			}
		}
		printSession(os.Stdout, resp.Session, time.Now())
		// The git section is extra: a daemon without it, or git failing,
		// never fails the status.
		if w, err := c.welcome(); err == nil && protocol.HasCapability(w.Capabilities, protocol.CapGitState) {
			st, err := c.gitState(id)
			if err != nil {
				st = &session.GitState{Error: err.Error()}
			}
			printGitState(os.Stdout, st, time.Now())
		}
		if withStats && resp.Session.Status == session.StatusRunning {
			stats, err := c.sessionStats(id)
			if err != nil {
				return err
			}
			printSessionStats(os.Stdout, stats)
		}
		return nil
	})
	status.Flags().BoolVar(&withStats, "stats", false, "Also show the session's resource usage (memory, CPU, threads, files)")
	cmds = append(cmds, annotate(status, "[OPTIONS] <SESSION_ID>", "<SESSION_ID>"))

	var diffStat, diffNameOnly bool
	diff := a.command("diff SESSION", "Show what a session changed in its git repository", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		return c.diff(id, diffStat, diffNameOnly)
	})
	diff.Flags().BoolVar(&diffStat, "stat", false, "Show a diffstat instead of the diff")
	diff.Flags().BoolVar(&diffNameOnly, "name-only", false, "Show only the changed files' names")
	diff.Annotations = map[string]string{"after": "Notes:\n  The diff covers everything since the session started: commits, staged and\n  unstaged changes, and untracked files. A session started outside a repository\n  (or before agentd recorded where it started) is compared with HEAD instead.\n  Git runs on the daemon's machine and agentd never changes the repository."}
	cmds = append(cmds, annotate(diff, "[OPTIONS] <SESSION_ID>", "<SESSION_ID>"))

	cmds = append(cmds, annotate(a.command("artifacts SESSION", "List what a session produced that can be downloaded", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		artifacts, err := c.listArtifacts(id)
		if err != nil {
			return err
		}
		printArtifacts(os.Stdout, artifacts)
		return nil
	}), "<SESSION_ID>", "<SESSION_ID>"))

	var artifactOutput string
	artifact := a.command("artifact SESSION NAME", "Download one of a session's artifacts", twoArgs, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		return c.downloadArtifact(id, args[1], artifactOutput)
	})
	artifact.Flags().StringVarP(&artifactOutput, "output", "o", "", "Write to FILE instead of stdout (only once complete)")
	withValue(artifact, "output", "FILE")
	cmds = append(cmds, annotate(artifact, "[OPTIONS] <SESSION_ID> <NAME>", "<SESSION_ID>\n<NAME>        diff, patch, history or history.vt (see `agent artifacts`)"))

	var ev eventsOptions
	events := a.command("events [SESSION]", "Show session events: lifecycle and attention", optionalArg, func(args []string) error {
		return a.events(firstArg(args), ev)
	})
	events.Flags().BoolVarP(&ev.follow, "follow", "f", false, "Keep printing events as they happen")
	events.Flags().IntVarP(&ev.lines, "lines", "n", 20, "How many past events to show first")
	events.Flags().StringVar(&ev.level, "level", "", "Only events at or above this attention: info, notice or action")
	events.Flags().BoolVar(&ev.json, "json", false, "Print one JSON object per event")
	events.Flags().BoolVarP(&ev.all, "all", "a", false, "Show the events of this machine and every host (see `agent hosts`)")
	events.Flags().BoolVar(&ev.notify, "notify", false, "Ring the bell and post a desktop notification for action-level events")
	events.Flags().StringVar(&ev.exec, "exec", "", "Run CMD with sh for each event, described in AGENTD_EVENT_* variables")
	events.Flags().Func("after", "Start after event ID (from an earlier run) instead of with the newest events", func(v string) error {
		id, err := strconv.ParseUint(v, 10, 64)
		ev.after, ev.afterSet = id, true
		return err
	})
	withValue(events, "lines", "N")
	withValue(events, "level", "LEVEL")
	withValue(events, "exec", "CMD")
	withValue(events, "after", "ID")
	events.Annotations = map[string]string{"after": `Examples:
  agent events                     the last 20 events of every session
  agent events fix-tests -f        follow one session
  agent --host devbox events -f --notify
                                   alert when a session on devbox needs you
  agent events -f --level action --exec 'say "$AGENTD_EVENT_SESSION needs you"'

--exec gets AGENTD_EVENT_ID, _HOST, _SESSION, _KIND, _ATTENTION, _SUMMARY and
_TIME. Following reconnects by itself and resumes after the last event shown.`}
	cmds = append(cmds, annotate(events, "[OPTIONS] [SESSION_ID]", "[SESSION_ID]"))

	cmds = append(cmds, group("workspace", "Manage named working directories on the daemon's machine",
		"Name directories on the daemon's machine, then start sessions in them\nwith `agent new --workspace NAME`.",
		"Notes:\n  Paths are resolved by the daemon; `~/` is its user's home. The directory must\n  already exist: agentd does not clone, create, or sync workspaces.\n  Removing a workspace does not affect sessions already running in it.",
		annotate(a.command("add NAME PATH", "Add a workspace", twoArgs, func(args []string) error {
			c, err := a.connect(nil, "", nil)
			if err != nil {
				return err
			}
			path := args[1]
			if host := c.remoteName(); host != "" {
				path, err = remoteWorkspacePath(host, path)
			} else {
				path, err = workspacePath(path)
			}
			if err != nil {
				return err
			}
			resp, err := c.call(&protocol.Request{AddWorkspace: &protocol.AddWorkspace{Name: args[0], Path: path}}, 0, func(r *protocol.Response) bool { return r.Workspace != nil })
			if err != nil {
				return err
			}
			fmt.Printf("added workspace %s at %s\n", resp.Workspace.Name, escapeControls(resp.Workspace.Path))
			return nil
		}), "<NAME> <PATH>", "<NAME>\n<PATH>"),
		visibleAlias(a.command("list", "List workspaces", noArgs, func([]string) error {
			c, err := a.connect(nil, "", nil)
			if err != nil {
				return err
			}
			resp, err := c.call(&protocol.Request{ListWorkspaces: protocol.Empty}, 0, func(r *protocol.Response) bool { return r.Workspaces != nil })
			if err != nil {
				return err
			}
			for _, w := range *resp.Workspaces {
				fmt.Printf("%s\t%s\n", w.Name, escapeControls(w.Path))
			}
			return nil
		}), "ls"),
		annotate(a.command("rm NAME", "Remove a workspace", oneArg, func(args []string) error {
			c, err := a.connect(nil, "", nil)
			if err != nil {
				return err
			}
			_, err = c.call(&protocol.Request{RemoveWorkspace: &protocol.WorkspaceRef{Name: args[0]}}, 0, func(r *protocol.Response) bool { return r.Ok != nil })
			return err
		}), "<NAME>", "<NAME>"),
	))

	cmds = append(cmds, group("daemon", "Inspect or control the local agent daemon",
		"Inspect, restart, or upgrade the local daemon process.",
		"Notes:\n  `restart` keeps running sessions; they live in their own worker processes\n  and reattach to the new daemon.\n  `upgrade` replaces the daemon and hands running sessions over to the new\n  binary without stopping their agents; attached clients reattach.",
		a.command("info", "Show daemon version, socket, pid, and compatibility", noArgs, func([]string) error {
			c, err := a.connect(nil, "info", nil)
			if err != nil {
				return err
			}
			return c.printDaemonStatus()
		}),
		a.command("stats", "Show the resource usage of the daemon and each running session", noArgs, func([]string) error {
			c, err := a.connect(nil, "", nil)
			if err != nil {
				return err
			}
			if err := c.requireStats(); err != nil {
				return err
			}
			return c.printDaemonStats(os.Stdout)
		}),
		a.command("restart", "Restart the daemon", noArgs, func([]string) error {
			c, err := a.connect(nil, "restart", nil)
			if err != nil {
				return err
			}
			if err := c.restartDaemon(); err != nil {
				return err
			}
			return c.printDaemonStatus()
		}),
		a.command("upgrade", "Upgrade the daemon and running sessions to the agentd binary", noArgs, func([]string) error {
			c, err := a.connect(nil, "upgrade", nil)
			if err != nil {
				return err
			}
			return c.upgradeDaemon()
		}),
	))

	var fingerprint string
	hostAdd := a.command("add NAME ADDRESS", "Add a host and pin its daemon key", twoArgs, func(args []string) error {
		return a.hostAdd(args[0], args[1], fingerprint)
	})
	hostAdd.Flags().StringVar(&fingerprint, "fingerprint", "", "Expected daemon key (from `agentd remote id` on the host); skips the prompt")
	withValue(hostAdd, "fingerprint", "FP")
	var noProbe bool
	listHosts := func([]string) error { return a.listHosts(noProbe) }
	hostList := visibleAlias(a.command("list", "List hosts and whether they are reachable", noArgs, listHosts), "ls")
	hostList.Flags().BoolVar(&noProbe, "no-probe", false, "List hosts.toml without contacting the hosts (shows their keys)")
	hosts := a.command("hosts", "List hosts and whether they are reachable (same as `agent host ls`)", noArgs, listHosts)
	hosts.Flags().BoolVar(&noProbe, "no-probe", false, "List hosts.toml without contacting the hosts (shows their keys)")
	cmds = append(cmds, hosts)
	cmds = append(cmds, group("host", "Manage remote hosts this machine can reach",
		"Remote hosts running agentd with `[remote] listen` set.",
		"Examples:\n  agent host add devbox 100.64.0.5:7433\n  agent host ls\n  agent --host devbox ls\n  agent attach devbox/auth-refactor\n  agent --host auto new --workspace mono fix-tests",
		annotate(hostAdd, "[OPTIONS] <NAME> <ADDRESS>", "<NAME>     \n<ADDRESS>  HOST:PORT of the remote agentd's QUIC listener"),
		hostList,
		annotate(a.command("info NAME", "Show a host's machine, daemon, and capabilities", oneArg, func(args []string) error {
			return a.hostInfo(args[0])
		}), "<NAME>", "<NAME>  A host from `agent host ls`, or `local`"),
		annotate(a.command("rm NAME", "Forget a host", oneArg, func(args []string) error {
			removed, err := transport.RemoveHost(a.paths.HostsPath(), args[0])
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("no host `%s`", args[0])
			}
			fmt.Printf("removed host `%s`\n", args[0])
			return nil
		}), "<NAME>", "<NAME>"),
	))

	cmds = append(cmds, group("remote", "Show this machine's key for remote access",
		"This machine's identity when connecting to remote hosts.", "",
		a.command("id", "Print this machine's key fingerprint", noArgs, func([]string) error {
			id, err := loadClientIdentity(a.paths)
			if err != nil {
				return err
			}
			fmt.Println(id.Fingerprint)
			return nil
		}),
	))

	var all []*cobra.Command
	var walk func(cmds []*cobra.Command)
	walk = func(cmds []*cobra.Command) {
		for _, cmd := range cmds {
			all = append(all, cmd)
			walk(cmd.Commands())
		}
	}
	walk(cmds)
	for _, cmd := range all {
		cmd.Flags().SortFlags = false
		cmd.Flags().BoolP("help", "h", false, "Print help")
	}
	return cmds
}

// listHosts prints every host with what probing it found, or with
// noProbe, hosts.toml as it is.
func (a *app) listHosts(noProbe bool) error {
	if noProbe {
		hosts, err := transport.ReadHosts(a.paths.HostsPath())
		if err != nil {
			return err
		}
		if len(hosts) == 0 {
			fmt.Println("no hosts; add one with `agent host add NAME HOST:PORT`")
		}
		for _, h := range hosts {
			fmt.Printf("%s\t%s\t%s\n", h.Name, h.Address, h.Fingerprint)
		}
		return nil
	}
	probes, err := a.probeHosts(false)
	if err != nil {
		return err
	}
	renderHosts(os.Stdout, probes)
	if len(probes) == 1 {
		fmt.Println("no remote hosts; add one with `agent host add NAME HOST:PORT`")
	}
	return nil
}

// hostInfo prints one host's machine and daemon.
func (a *app) hostInfo(name string) error {
	p := &hostProbe{name: localHostName, address: a.paths.Socket}
	if name != localHostName {
		host, err := transport.LookupHost(a.paths.HostsPath(), name)
		if err != nil {
			return err
		}
		p = &hostProbe{name: host.Name, address: host.Address, host: &host}
	}
	a.probe(p, false)
	renderHostInfo(os.Stdout, p)
	return nil
}

// placeSession resolves `agent new --host auto` to a host, telling the user
// which and why. A session placed on another machine needs a directory that
// means the same there: a workspace, or an absolute or ~/ --cwd.
func (a *app) placeSession(agent, workspace, cwd string) error {
	if workspace == "" && !(strings.HasPrefix(cwd, "/") || cwd == "~" || strings.HasPrefix(cwd, "~/")) {
		return errors.New("`--host auto` needs --workspace NAME or an absolute or ~/ --cwd, which mean the same on every host")
	}
	// Placement may pick this machine, whose daemon is started like any
	// command's.
	local := &client{paths: a.paths}
	err := local.ensureDaemon()
	local.close()
	if err != nil {
		return err
	}
	probes, err := a.probeHosts(workspace != "")
	if err != nil {
		return err
	}
	p, err := choosePlacement(probes, agent, workspace)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "placing the session on %s (%s)\n", p.probe.name, p.reason)
	a.host = p.probe.name
	if p.probe.host == nil {
		a.host = ""
	}
	return nil
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (a *app) runRuntime(id string) error {
	c := a.client
	if c == nil {
		var err error
		if c, err = a.connect(nil, "", nil); err != nil {
			return err
		}
	}
	if id == "" {
		picked, err := c.pickSession()
		if err != nil || picked == "" {
			return err
		}
		id = picked
	}
	return c.attachSession(id)
}

func (a *app) kill(id string, remove bool) error {
	c, err := a.connect([]*string{&id}, "", nil)
	if err != nil {
		return err
	}
	resp, err := c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: remove}}, 0, func(r *protocol.Response) bool { return r.KillSession != nil })
	if err != nil {
		return err
	}
	if resp.KillSession.WasRunning {
		fmt.Printf("terminated session %s\n", id)
	}
	if resp.KillSession.Removed {
		fmt.Printf("removed session %s\n", id)
	}
	return nil
}

func (c *client) listSessions(timeout time.Duration) ([]session.Record, error) {
	resp, err := c.call(&protocol.Request{ListSessions: protocol.Empty}, timeout, func(r *protocol.Response) bool { return r.Sessions != nil })
	if err != nil {
		return nil, err
	}
	return *resp.Sessions, nil
}

func (c *client) printDaemonStatus() error {
	s, err := c.managementStatus()
	if err != nil {
		return err
	}
	fmt.Println("source: daemon_management")
	fmt.Printf("daemon_version: %s\n", s.DaemonVersion)
	fmt.Printf("daemon_protocol_version: %d\n", s.ProtocolVersion)
	fmt.Printf("client_version: %s\n", Version)
	fmt.Printf("expected_protocol_version: %d\n", protocol.ProtocolVersion)
	fmt.Printf("pid: %d\n", s.PID)
	fmt.Printf("root: %s\n", s.Root)
	fmt.Printf("socket: %s\n", s.Socket)
	fmt.Printf("running_sessions: %t\n", s.RunningSessions)
	switch {
	case s.Remote != "":
		fmt.Printf("remote: %s\n", s.Remote)
	case s.RemoteError != "":
		fmt.Printf("remote: not listening (%s)\n", s.RemoteError)
	default:
		fmt.Println("remote: off")
	}
	fmt.Printf("open_streams: %d\n", s.OpenStreams)
	printConnections(os.Stdout, s.Connections, time.Now())
	return nil
}

// printConnections lists remote connections, one per line.
func printConnections(w io.Writer, conns []protocol.ManagementConnection, now time.Time) {
	for _, c := range conns {
		who := c.Fingerprint
		if c.Name != "" {
			who = c.Name + " " + c.Fingerprint
		}
		fmt.Fprintf(w, "connection: %s from %s, up %s, streams %d open/%d total, rtt %s, sent %s, received %s, lost %d/%d packets\n",
			escapeControls(who), c.Remote, formatElapsed(int64(now.Sub(c.ConnectedAt).Seconds())), c.StreamsOpen, c.StreamsOpened,
			(time.Duration(c.RTTMicros) * time.Microsecond).Round(10*time.Microsecond), formatBytes(c.BytesSent), formatBytes(c.BytesReceived),
			c.PacketsLost, c.PacketsSent)
	}
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printSession(w io.Writer, s *session.Record, now time.Time) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }
	p("name: %s\n", s.SessionID)
	if s.UID != "" {
		p("uid: %s\n", s.UID)
	}
	p("agent: %s\n", s.Agent)
	if s.Model != nil {
		p("model: %s\n", *s.Model)
	}
	p("status: %s\n", s.Status)
	p("elapsed: %s\n", elapsedLabel(s, now))
	if s.Activity != session.ActivityUnknown {
		p("activity: %s\n", activityText(s, now))
	}
	if s.Foreground != nil {
		p("foreground: %s\n", escapeControls(*s.Foreground))
	}
	if s.Title != nil {
		p("title: %s\n", escapeControls(*s.Title))
	}
	if s.LastOutputAt != nil {
		p("last_output: %s ago\n", formatElapsed(int64(now.Sub(*s.LastOutputAt)/time.Second)))
	}
	p("attention: %s\n", s.Attention)
	if s.AttentionSummary != nil {
		p("attention_summary: %s\n", escapeControls(*s.AttentionSummary))
	}
	if s.AttentionAt != nil {
		p("attention_since: %s ago\n", formatElapsed(max(int64(now.Sub(*s.AttentionAt)/time.Second), 0)))
	}
	p("cwd: %s\n", escapeControls(s.Cwd))
	if s.Workspace != nil {
		p("workspace: %s\n", escapeControls(*s.Workspace))
	}
	if s.WorkerPID != nil {
		p("worker_pid: %d\n", *s.WorkerPID)
	}
	if s.AgentPID != nil {
		p("agent_pid: %d\n", *s.AgentPID)
	}
	if s.ExitCode != nil {
		p("exit_code: %d\n", *s.ExitCode)
	}
	if s.Error != nil {
		p("error: %s\n", escapeControls(*s.Error))
	}
}

// newSessionRequest resolves `agent new`'s arguments.
func (c *client) newSessionRequest(name, cwd, agent, workspace string) (*protocol.CreateSession, error) {
	req := &protocol.CreateSession{}
	if workspace = strings.TrimSpace(workspace); workspace != "" {
		req.Workspace = &workspace
	}
	requested, err := normalizeRequestedName(name)
	if err != nil {
		return nil, err
	}
	if requested != "" {
		req.Name = &requested
	}
	switch {
	case workspace != "":
		req.Cwd, err = workspaceRelativeCwd(workspace, cwd)
	case c.host != nil:
		req.Cwd, err = remoteCwd(c.host.Name, cwd)
	default:
		req.Cwd, err = resolveCwd(cwd)
	}
	if err != nil {
		return nil, err
	}
	// A remote daemon picks its own default agent.
	req.Agent = agent
	if agent == "" && c.host == nil {
		cfg, err := config.Load(c.paths.Config)
		if err != nil {
			return nil, err
		}
		req.Agent = cfg.DefaultAgent
	}
	return req, nil
}

func normalizeRequestedName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name != "" && !session.ValidName(name) {
		return "", fmt.Errorf("invalid session name `%s`: %s", name, session.NameRules)
	}
	return name, nil
}

// remoteCwd checks a remote session's --cwd without --workspace. It is a
// path on the remote machine, absolute or ~/..., so it cannot be checked
// here; the daemon validates it.
func remoteCwd(host, cwd string) (string, error) {
	switch {
	case cwd == "":
		return "", fmt.Errorf("`agent new` on `%s` needs --workspace NAME or --cwd DIR (a directory on that machine)", host)
	case !(strings.HasPrefix(cwd, "/") || cwd == "~" || strings.HasPrefix(cwd, "~/")):
		return "", fmt.Errorf("--cwd `%s` must be absolute or start with `~/` on `%s`", cwd, host)
	}
	return cwd, nil
}

// workspaceRelativeCwd checks --cwd with --workspace: a directory under the
// workspace root on the daemon's machine, sent as given for the daemon to
// resolve. Absolute paths are refused early for a clearer error.
func workspaceRelativeCwd(workspace, cwd string) (string, error) {
	if strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, "~") {
		return "", fmt.Errorf("working directory `%s` must be relative to workspace `%s`", cwd, workspace)
	}
	return cwd, nil
}

// resolveCwd resolves the directory a new session runs in: cwd when given,
// else the current directory. The daemon only checks that it exists, so the
// client sends an absolute, canonical path and fails early with a clear
// error.
func resolveCwd(cwd string) (string, error) {
	requested := cwd
	if requested == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to resolve current directory: %w", err)
		}
		requested = wd
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("failed to resolve `%s`: %w", requested, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("working directory `%s` does not exist", requested)
	case err != nil:
		return "", fmt.Errorf("failed to resolve `%s`: %w", requested, err)
	}
	if info, err := os.Stat(canonical); err != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory `%s` is not a directory", requested)
	}
	return canonical, nil
}

// workspacePath is the path sent for `agent workspace add`. A ~ path is sent
// as given, for the daemon to expand against its own home. Anything else is
// resolved here, so `agent workspace add mono .` works; that relies on the
// CLI sharing the daemon's filesystem.
func workspacePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		return path, nil
	}
	return resolveCwd(path)
}

// remoteWorkspacePath checks a remote workspace's path, which is on the
// remote machine: absolute, or ~/... for its user's home.
func remoteWorkspacePath(host, path string) (string, error) {
	if !(strings.HasPrefix(path, "/") || path == "~" || strings.HasPrefix(path, "~/")) {
		return "", fmt.Errorf("workspace path `%s` must be absolute or start with `~/` on `%s`", path, host)
	}
	return path, nil
}

func resolveDetachSessionID(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	if env := os.Getenv("AGENTD_SESSION_ID"); env != "" {
		return env, nil
	}
	return "", errors.New("`agent detach` without a session id only works inside a managed session")
}

type sendInputArgs struct {
	session, source, host string
	data                  []string
}

// parseSendInput parses `agent send-input`'s arguments by hand: DATA is
// everything after the session, flag-like words included.
func parseSendInput(args []string) (sendInputArgs, error) {
	var in sendInputArgs
	value := func(i *int, arg, flag string) (string, bool, error) {
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			return v, true, nil
		}
		if arg != flag {
			return "", false, nil
		}
		if *i+1 >= len(args) {
			return "", true, usageError{fmt.Errorf("a value is required for '%s <%s>' but none was supplied", flag, strings.ToUpper(strings.TrimPrefix(flag, "--")))}
		}
		*i++
		return args[*i], true, nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if in.session != "" {
			if arg == "--" {
				i++
			}
			in.data = args[i:]
			break
		}
		switch arg {
		case "-h", "--help":
			return in, errHelp
		case "--":
			if i+1 < len(args) {
				in.session = args[i+1]
				in.data = args[min(i+2, len(args)):]
			}
			i = len(args)
			continue
		}
		if v, ok, err := value(&i, arg, "--source-session-id"); ok || err != nil {
			in.source = v
			if err != nil {
				return in, err
			}
			continue
		}
		if v, ok, err := value(&i, arg, "--host"); ok || err != nil {
			in.host = v
			if err != nil {
				return in, err
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			return in, usageError{fmt.Errorf("unexpected argument '%s' found", arg)}
		}
		in.session = arg
	}
	if in.session == "" || len(in.data) == 0 {
		return in, usageError{errors.New("the following required arguments were not provided: <SESSION_ID> <DATA>...")}
	}
	return in, nil
}

// errHelp asks for a command's help, from a command that parses its own
// arguments.
var errHelp = errors.New("help requested")

// hostAdd adds a remote host, pinning the key its daemon presents now.
func (a *app) hostAdd(name, address, fingerprint string) error {
	path := a.paths.HostsPath()
	if err := transport.ValidHostName(name); err != nil {
		return err
	}
	if _, err := transport.LookupHost(path, name); err == nil {
		return fmt.Errorf("host `%s` already exists; remove it first with `agent host rm %s`", name, name)
	}
	if fingerprint != "" && !transport.ValidFingerprint(fingerprint) {
		return fmt.Errorf("`%s` is not a key fingerprint (expected SHA256:...)", fingerprint)
	}
	id, err := loadClientIdentity(a.paths)
	if err != nil {
		return err
	}
	presented, err := transport.ProbeFingerprint(context.Background(), address, id)
	if err != nil {
		return fmt.Errorf("could not reach agentd at %s: %w", address, err)
	}
	switch {
	case fingerprint != "" && fingerprint != presented:
		return fmt.Errorf("%s presented key %s, not the expected %s; not adding `%s`", address, presented, fingerprint, name)
	case fingerprint == "":
		fmt.Printf("The agentd at %s presents this key:\n  %s\nCompare it with `agentd remote id` on that machine.\n", address, presented)
		ok, err := confirm("Trust this key?")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("not adding `%s`", name)
		}
	}
	if err := transport.AddHost(path, transport.Host{Name: name, Address: address, Fingerprint: presented}); err != nil {
		return err
	}
	fmt.Printf("added host `%s` (%s)\n\nIf this machine is not authorized there yet, run on `%s`:\n  agentd remote authorize %s %s\n",
		name, address, name, id.Fingerprint, localHostname())
	return nil
}

// confirm asks a yes/no question on the terminal. Without one it refuses,
// since trust must never be granted by default.
func confirm(question string) (bool, error) {
	if !isTerminal(os.Stdin) {
		return false, errors.New("no terminal to confirm on; pass --fingerprint (from `agentd remote id` on the host)")
	}
	fmt.Printf("%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(answer) {
	case "y", "Y", "yes", "Yes":
		return true, nil
	}
	return false, nil
}
