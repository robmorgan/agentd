package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// parse finds the command args name and parses its flags, as Execute
// would, without running it.
func parse(t *testing.T, args ...string) (*cobra.Command, []string) {
	t.Helper()
	root := (&app{}).rootCommand()
	cmd, rest, err := root.Find(args)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return cmd, cmd.Flags().Args()
}

func TestCommandsParse(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		path  string
		pos   []string
		flags map[string]string
	}{
		{[]string{"new", "fix-failing-tests"}, "agent new", []string{"fix-failing-tests"}, nil},
		{[]string{"new", "--cwd", "/tmp", "--agent", "codex", "--workspace", "mono", "x"}, "agent new", []string{"x"}, map[string]string{"cwd": "/tmp", "agent": "codex", "workspace": "mono"}},
		{[]string{"rm", "demo"}, "agent rm", []string{"demo"}, nil},
		{[]string{"kill", "--rm", "demo"}, "agent kill", []string{"demo"}, map[string]string{"rm": "true"}},
		{[]string{"detach"}, "agent detach", nil, nil},
		{[]string{"detach", "demo", "--attach", "a1"}, "agent detach", []string{"demo"}, map[string]string{"attach": "a1"}},
		{[]string{"attachments", "demo"}, "agent attachments", []string{"demo"}, nil},
		{[]string{"history", "demo", "--vt"}, "agent history", []string{"demo"}, map[string]string{"vt": "true"}},
		{[]string{"ls"}, "agent list", nil, nil},
		{[]string{"sessions"}, "agent list", nil, nil},
		{[]string{"workspace", "add", "mono", "~/src"}, "agent workspace add", []string{"mono", "~/src"}, nil},
		{[]string{"workspace", "ls"}, "agent workspace list", nil, nil},
		{[]string{"daemon", "restart"}, "agent daemon restart", nil, nil},
		{[]string{"daemon", "stats"}, "agent daemon stats", nil, nil},
		{[]string{"status", "--stats", "demo"}, "agent status", []string{"demo"}, map[string]string{"stats": "true"}},
		{[]string{"daemon", "upgrade"}, "agent daemon upgrade", nil, nil},
		{[]string{"host", "add", "dev", "10.0.0.1:7433", "--fingerprint", "SHA256:x"}, "agent host add", []string{"dev", "10.0.0.1:7433"}, map[string]string{"fingerprint": "SHA256:x"}},
		{[]string{"--host", "dev", "ls"}, "agent list", nil, map[string]string{"host": "dev"}},
		{[]string{"runtime", "demo"}, "agent runtime", []string{"demo"}, nil},
		{[]string{"diff", "demo"}, "agent diff", []string{"demo"}, nil},
		{[]string{"diff", "--stat", "dev/demo"}, "agent diff", []string{"dev/demo"}, map[string]string{"stat": "true"}},
		{[]string{"--host", "dev", "diff", "--name-only", "demo"}, "agent diff", []string{"demo"}, map[string]string{"name-only": "true", "host": "dev"}},
		{[]string{"artifacts", "demo"}, "agent artifacts", []string{"demo"}, nil},
		{[]string{"artifact", "demo", "patch", "-o", "out.mbox"}, "agent artifact", []string{"demo", "patch"}, map[string]string{"output": "out.mbox"}},
	} {
		cmd, pos := parse(t, tc.args...)
		if cmd.CommandPath() != tc.path || strings.Join(pos, " ") != strings.Join(tc.pos, " ") {
			t.Errorf("%v: %s %v", tc.args, cmd.CommandPath(), pos)
		}
		for name, want := range tc.flags {
			if got := cmd.Flags().Lookup(name).Value.String(); got != want {
				t.Errorf("%v: --%s = %q, want %q", tc.args, name, got, want)
			}
		}
	}
}

func TestSendInputArguments(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		session, data string
		source        string
	}{
		{[]string{"demo", "hello", "world"}, "demo", "hello world", ""},
		{[]string{"demo", "-n", "--flag", "x"}, "demo", "-n --flag x", ""},
		{[]string{"--source-session-id", "other", "demo", "hi"}, "demo", "hi", "other"},
		{[]string{"--source-session-id=other", "demo", "--", "-x"}, "demo", "-x", "other"},
	} {
		in, err := parseSendInput(tc.args)
		if err != nil || in.session != tc.session || strings.Join(in.data, " ") != tc.data || in.source != tc.source {
			t.Errorf("%v: %+v %v", tc.args, in, err)
		}
	}
	var usage usageError
	if _, err := parseSendInput([]string{"demo"}); !errors.As(err, &usage) {
		t.Errorf("missing data: %v", err)
	}
	if _, err := parseSendInput([]string{"-h"}); !errors.Is(err, errHelp) {
		t.Errorf("-h: %v", err)
	}
}

func TestHelp(t *testing.T) {
	var root bytes.Buffer
	writeHelp(&root, (&app{}).rootCommand())
	help := root.String()
	for _, want := range []string{"agentd - agent multiplexer", "agent 0.1.0", "list         List known sessions [aliases: ls]", "-V, --version"} {
		if !strings.Contains(help, want) {
			t.Errorf("root help lacks %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "\n  runtime") || strings.Contains(help, "\n  sessions") {
		t.Errorf("root help shows hidden commands:\n%s", help)
	}
	daemon, _ := parse(t, "daemon")
	var b bytes.Buffer
	writeHelp(&b, daemon)
	if strings.Contains(b.String(), "agentd - agent multiplexer") || !strings.Contains(b.String(), "agent daemon") {
		t.Errorf("daemon help:\n%s", b.String())
	}
}

func TestMainExitStatus(t *testing.T) {
	t.Setenv("AGENTD_DIR", filepath.Join(t.TempDir(), "root"))
	if got := Main([]string{"no-such-command"}); got != 2 {
		t.Errorf("unknown command: %d", got)
	}
	if got := Main([]string{"attach"}); got != 2 {
		t.Errorf("missing argument: %d", got)
	}
	if got := Main([]string{"--version"}); got != 0 {
		t.Errorf("--version: %d", got)
	}
}

func TestSessionAddressesSelectAHost(t *testing.T) {
	a, b := "devbox/auth", "devbox/other"
	if host, err := takeSessionHost([]*string{&a, &b}); err != nil || host != "devbox" || a != "auth" || b != "other" {
		t.Fatalf("%q %v %q %q", host, err, a, b)
	}
	plain := "auth"
	if host, err := takeSessionHost([]*string{&plain, nil}); err != nil || host != "" || plain != "auth" {
		t.Fatalf("plain: %q %v", host, err)
	}
	for _, bad := range []string{"/auth", "devbox/"} {
		id := bad
		if _, err := takeSessionHost([]*string{&id}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	x, y := "one/a", "two/b"
	if _, err := takeSessionHost([]*string{&x, &y}); err == nil || !strings.Contains(err.Error(), "different hosts") {
		t.Fatalf("conflict: %v", err)
	}
}

func TestResolveCwd(t *testing.T) {
	dir := t.TempDir()
	canonical, _ := filepath.EvalSymlinks(dir)
	if got, err := resolveCwd(dir); err != nil || got != canonical {
		t.Fatalf("%q %v", got, err)
	}
	t.Chdir(dir)
	if got, err := resolveCwd(""); err != nil || got != canonical {
		t.Fatalf("current directory: %q %v", got, err)
	}
	missing := filepath.Join(dir, "missing")
	if _, err := resolveCwd(missing); err == nil || err.Error() != "working directory `"+missing+"` does not exist" {
		t.Fatalf("missing: %v", err)
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := resolveCwd(file); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file: %v", err)
	}
}

func TestNewSessionRequest(t *testing.T) {
	c := testClient(t)
	dir := t.TempDir()
	t.Chdir(dir)
	canonical, _ := filepath.EvalSymlinks(dir)
	req, err := c.newSessionRequest("", "", "", "")
	if err != nil || req.Cwd != canonical || req.Agent != "claude" || req.Name != nil || req.Workspace != nil {
		t.Fatalf("defaults: %+v %v", req, err)
	}
	os.MkdirAll(c.paths.Root, 0o700)
	os.WriteFile(c.paths.Config, []byte("default_agent = \"codex\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	if req, _ := c.newSessionRequest("", "", "", ""); req.Agent != "codex" {
		t.Fatalf("configured default: %q", req.Agent)
	}
	req, err = c.newSessionRequest(" fix ", dir, "zed", "")
	if err != nil || *req.Name != "fix" || req.Agent != "zed" {
		t.Fatalf("explicit: %+v %v", req, err)
	}
	if _, err := c.newSessionRequest("Bad", "", "", ""); err == nil || !strings.Contains(err.Error(), "invalid session name `Bad`") {
		t.Fatalf("bad name: %v", err)
	}
	// With a workspace the cwd is relative and sent as given.
	req, err = c.newSessionRequest("", "sub/dir", "", " mono ")
	if err != nil || req.Cwd != "sub/dir" || *req.Workspace != "mono" {
		t.Fatalf("workspace: %+v %v", req, err)
	}
	if _, err := c.newSessionRequest("", "/abs", "", "mono"); err == nil {
		t.Fatal("absolute cwd in a workspace accepted")
	}
}

func TestRemotePaths(t *testing.T) {
	for cwd, ok := range map[string]bool{"/srv/x": true, "~": true, "~/x": true, "x": false, "~other": false, "": false} {
		if _, err := remoteCwd("devbox", cwd); (err == nil) != ok {
			t.Errorf("remoteCwd %q: %v", cwd, err)
		}
		if cwd == "" {
			continue
		}
		if _, err := remoteWorkspacePath("devbox", cwd); (err == nil) != ok {
			t.Errorf("remoteWorkspacePath %q: %v", cwd, err)
		}
	}
	if got, err := workspacePath("~/src"); err != nil || got != "~/src" {
		t.Fatalf("~ path: %q %v", got, err)
	}
	dir := t.TempDir()
	canonical, _ := filepath.EvalSymlinks(dir)
	if got, err := workspacePath(dir); err != nil || got != canonical {
		t.Fatalf("resolved: %q %v", got, err)
	}
}

func TestDetachSessionID(t *testing.T) {
	t.Setenv("AGENTD_SESSION_ID", "")
	if _, err := resolveDetachSessionID(""); err == nil {
		t.Fatal("no session outside a managed session")
	}
	t.Setenv("AGENTD_SESSION_ID", "inside")
	if got, _ := resolveDetachSessionID(""); got != "inside" {
		t.Fatalf("from env: %q", got)
	}
	if got, _ := resolveDetachSessionID("explicit"); got != "explicit" {
		t.Fatalf("explicit: %q", got)
	}
}

func TestDaemonExecutable(t *testing.T) {
	if got, _ := daemonExecutableFrom("/custom/agentd", "/bin/agent", nil); got != "/custom/agentd" {
		t.Fatalf("AGENTD_BIN: %q", got)
	}
	if got, _ := daemonExecutableFrom("", "/opt/bin/agent", nil); got != "/opt/bin/agentd" {
		t.Fatalf("next to agent: %q", got)
	}
}
