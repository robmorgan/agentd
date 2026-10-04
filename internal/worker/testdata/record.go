//go:build ignore

// record captures representative PTY output streams for the worker's terminal
// fidelity tests. See streams/README.md for what each stream exercises and how
// to re-record one:
//
//	go run internal/worker/testdata/record.go [-only NAME] [-scrub STRING]...
//
// Each program runs under a real PTY at a fixed size in a scratch directory.
// The output is fed through libghostty-vt so terminal queries (cursor
// position, device attributes, colors) get answers, the way the worker
// answers them, and so the script can wait for text to appear on screen.
// The raw bytes are saved gzip-compressed in streams/NAME.vt.gz after
// replacing identifying strings with same-length placeholders.
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	cols = 120
	rows = 40
	// workDir is fixed rather than random so re-recording gives the same
	// paths in the output.
	workDir = "/tmp/agentd-demo"
)

// step is one scripted action: wait until want is on screen (if set), then
// send keys (if set), then pause.
type step struct {
	want string
	// only, if set, skips the step unless the screen shows this text (for
	// prompts that appear only on a first run, such as folder trust).
	only  string
	send  string
	pause time.Duration
}

type scenario struct {
	name  string
	argv  []string
	env   []string
	setup func(dir string) error
	steps []step
}

func main() {
	only := flag.String("only", "", "record only this stream")
	flag.BoolVar(&verbose, "v", false, "print the screen after each step")
	out := flag.String("out", "internal/worker/testdata/streams", "output directory")
	var extra multiFlag
	flag.Var(&extra, "scrub", "an extra string to replace in the output (repeatable)")
	flag.Parse()

	secrets := identifyingStrings(extra)
	for _, sc := range scenarios() {
		if *only != "" && sc.name != *only {
			continue
		}
		data, err := record(sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", sc.name, err)
			os.Exit(1)
		}
		data = scrub(data, secrets)
		for _, s := range secrets {
			if bytes.Contains(asciiLower(data), asciiLower([]byte(s))) {
				fmt.Fprintf(os.Stderr, "%s: output still contains %q\n", sc.name, s)
				os.Exit(1)
			}
		}
		path := filepath.Join(*out, sc.name+".vt.gz")
		if err := writeGzip(path, data); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", sc.name, err)
			os.Exit(1)
		}
		fmt.Printf("%s: %d bytes\n", path, len(data))
	}
}

var verbose bool

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func scenarios() []scenario {
	sh := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8", "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"}
	return []scenario{
		{
			name: "shell",
			argv: []string{"/bin/bash", "--norc", "--noprofile", "-i"},
			env: append(sh, "HOME="+workDir, "BASH_SILENCE_DEPRECATION_WARNING=1", "CLICOLOR=1", "CLICOLOR_FORCE=1",
				`PS1=\[\e[1;32m\]demo\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ `),
			setup: setupRepo,
			steps: []step{
				{want: "demo:", send: "ls -G\r"},
				{want: "README", send: "git log --graph --color --oneline --decorate --all\r"},
				{want: "initial", send: "printf '\\e[1mbold\\e[0m \\e[3mitalic\\e[0m \\e[4munderline\\e[0m \\e[38;5;208m256\\e[0m \\e[38;2;10;200;120mtruecolor\\e[0m\\n'\r"},
				// Prompt redraws: edit a line in place, recall history, clear.
				{want: "truecolor", send: "echo this line gets edited", pause: 200 * time.Millisecond},
				{send: "\x15echo replaced\x1b[D\x1b[D\x7f\x7fXY\r", pause: 300 * time.Millisecond},
				{send: "\x1b[A", pause: 200 * time.Millisecond},
				{send: "\x1b[A\x1b[B\x03", pause: 200 * time.Millisecond},
				{send: "for i in $(seq 1 60); do echo \"row $i $(printf '%*s' $i '' | tr ' ' '=')\"; done\r", pause: 500 * time.Millisecond},
				{send: "echo " + strings.Repeat("wrapping-", 30) + "\r", pause: 300 * time.Millisecond},
				{send: "\x0c", pause: 300 * time.Millisecond},
				{send: "exit\r"},
			},
		},
		{
			name:  "vim",
			argv:  []string{"vim", "--clean", "-N", "main.go"},
			env:   append(sh, "HOME="+workDir),
			setup: setupSource,
			steps: editorSteps,
		},
		{
			name:  "nvim",
			argv:  []string{"nvim", "--clean", "main.go"},
			env:   append(sh, "HOME="+workDir, "XDG_CONFIG_HOME="+workDir+"/.config", "XDG_STATE_HOME="+workDir+"/.state", "XDG_DATA_HOME="+workDir+"/.data"),
			setup: setupSource,
			steps: editorSteps,
		},
		{
			name:  "go-test",
			argv:  []string{"go", "test", "-v", "-count=1", "./..."},
			env:   userEnv(),
			setup: setupGoTests,
			steps: []step{{pause: 0}},
		},
		{
			name:  "clang",
			argv:  []string{"clang", "-fsyntax-only", "-fcolor-diagnostics", "-ferror-limit=0", "-Wall", "-Wextra", "broken.c"},
			env:   sh,
			setup: setupBrokenC,
			steps: []step{{pause: 0}},
		},
		{
			// Claude Code with the user's own login: startup, the /help
			// screen, and quitting. Nothing is sent to the model.
			name:  "claude",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: []step{
				{want: "❯", pause: 1500 * time.Millisecond},
				// Accept the folder trust prompt, if shown.
				{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
				{only: "trust this folder", send: "\r", pause: 3 * time.Second},
				{send: "/help", pause: time.Second},
				{send: "\r", pause: 2 * time.Second},
				{send: "\x1b", pause: time.Second},
				// Quit with Ctrl-C twice rather than /exit, whose completion
				// list would show the recording user's own commands.
				{send: "\x03", pause: 300 * time.Millisecond},
				{send: "\x03", pause: 2 * time.Second},
			},
		},
		{
			// Codex with the user's own login: startup, the slash command
			// popup, the local /model picker, and quitting.
			name:  "codex",
			argv:  []string{"codex", "-c", "check_for_update_on_startup=false"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: []step{
				{want: "›", pause: 2 * time.Second},
				{send: "/", pause: time.Second},
				{send: "mod", pause: time.Second},
				{send: "\r", pause: 2 * time.Second},
				{send: "\x1b", pause: time.Second},
				{send: "\x03", pause: 2 * time.Second},
			},
		},
	}
}

// userEnv is a minimal environment around the recording user's real home
// directory, so Claude Code and Codex find their logins and settings but
// nothing else from the recording shell leaks in.
func userEnv() []string {
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8", "DISABLE_AUTOUPDATER=1"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

var editorSteps = []step{
	{want: "package main", pause: 300 * time.Millisecond},
	{send: "G", pause: 300 * time.Millisecond},
	{send: "gg", pause: 300 * time.Millisecond},
	{send: "/func\r", pause: 300 * time.Millisecond},
	{send: "ototal := 0 // edited\x1b", pause: 300 * time.Millisecond},
	{send: "\x04\x04", pause: 300 * time.Millisecond}, // scroll down half a page twice
	{send: "\x15", pause: 300 * time.Millisecond},     // and back up
	{send: ":split\r", pause: 300 * time.Millisecond},
	{send: ":vsplit\r", pause: 300 * time.Millisecond},
	{send: "\x17w50G", pause: 300 * time.Millisecond},
	{send: ":qa!\r"},
}

func setupEmpty(string) error { return nil }

func setupRepo(dir string) error {
	for name, body := range map[string]string{
		"README.md": "# demo\n", "main.go": "package main\n", "notes.txt": "notes\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, d := range []string{"cmd", "internal", "scripts"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "build.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		return err
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=" + dir, "PATH=/opt/homebrew/bin:/usr/bin:/bin",
			"GIT_AUTHOR_NAME=Demo", "GIT_AUTHOR_EMAIL=demo@example.com",
			"GIT_COMMITTER_NAME=Demo", "GIT_COMMITTER_EMAIL=demo@example.com",
			"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return nil
	}
	steps := [][]string{
		{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "initial import"},
		{"checkout", "-qb", "feature"}, {"commit", "-q", "--allow-empty", "-m", "add the feature"},
		{"commit", "-q", "--allow-empty", "-m", "fix review comments"},
		{"checkout", "-q", "main"}, {"commit", "-q", "--allow-empty", "-m", "unrelated fix on main"},
		{"merge", "-q", "--no-ff", "-m", "merge feature", "feature"},
		{"commit", "-q", "--allow-empty", "-m", "release 0.1.0"}, {"tag", "v0.1.0"},
	}
	for _, s := range steps {
		if err := git(s...); err != nil {
			return err
		}
	}
	return nil
}

func setupSource(dir string) error {
	var b strings.Builder
	b.WriteString("package main\n\nimport \"fmt\"\n\n")
	for i := range 40 {
		fmt.Fprintf(&b, "// step%d prints its number and returns it doubled.\nfunc step%d(n int) int {\n\tfmt.Println(\"step\", %d, n)\n\treturn n * 2\n}\n\n", i, i, i)
	}
	b.WriteString("func main() {\n\tn := 1\n")
	for i := range 40 {
		fmt.Fprintf(&b, "\tn = step%d(n)\n", i)
	}
	b.WriteString("}\n")
	return os.WriteFile(filepath.Join(dir, "main.go"), []byte(b.String()), 0o644)
}

func setupGoTests(dir string) error {
	files := map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.22\n",
		"calc/calc.go": `package calc

func Add(a, b int) int { return a + b }
func Div(a, b int) int { return a / b }
`,
		"calc/calc_test.go": `package calc

import (
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	for _, c := range []struct{ a, b, want int }{{1, 2, 3}, {2, 2, 4}, {-1, 1, 0}} {
		t.Run("", func(t *testing.T) {
			if got := Add(c.a, c.b); got != c.want {
				t.Fatalf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestAddIsWrong(t *testing.T) {
	t.Log("checking a deliberately wrong expectation")
	if got := Add(2, 2); got != 5 {
		t.Errorf("Add(2, 2) = %d, want 5", got)
	}
}

func TestSlow(t *testing.T) {
	time.Sleep(150 * time.Millisecond)
}

func TestSkipped(t *testing.T) { t.Skip("not on this platform") }

func TestDivPanics(t *testing.T) {
	Div(1, 0)
}
`,
		"text/text_test.go": `package text

import (
	"strings"
	"testing"
)

func TestUpper(t *testing.T) {
	for _, w := range []string{"alpha", "beta", "gamma", "delta"} {
		t.Run(w, func(t *testing.T) {
			if strings.ToUpper(w) == w {
				t.Fatal("unchanged")
			}
		})
	}
}
`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// setupBrokenC writes a C file with a few thousand diagnostics of varied
// kinds, so clang prints several hundred KB of colored output.
func setupBrokenC(dir string) error {
	var b strings.Builder
	b.WriteString("#include <stdio.h>\n\n")
	for i := range 250 {
		fmt.Fprintf(&b, "static int unused_%d(int a, int b) {\n", i)
		fmt.Fprintf(&b, "    int x%d = \"not an int\";\n", i)
		fmt.Fprintf(&b, "    undeclared_%d(a);\n", i)
		fmt.Fprintf(&b, "    if (a = b) printf(\"%%s\\n\", a);\n")
		fmt.Fprintf(&b, "    struct missing_%d s;\n", i)
		b.WriteString("}\n\n")
	}
	return os.WriteFile(filepath.Join(dir, "broken.c"), []byte(b.String()), 0o644)
}

// record runs one scenario and returns everything it wrote to the PTY.
func record(sc scenario) ([]byte, error) {
	if err := os.RemoveAll(workDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)
	if err := sc.setup(workDir); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}

	var replies [][]byte
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			replies = append(replies, append([]byte(nil), data...))
		}),
		libghostty.WithSizeReport(func(*libghostty.Terminal) (libghostty.SizeReportSize, bool) {
			return libghostty.SizeReportSize{Rows: rows, Columns: cols, CellWidth: 8, CellHeight: 16}, true
		}),
	)
	if err != nil {
		return nil, err
	}
	defer term.Close()

	cmd := exec.Command(sc.argv[0], sc.argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = sc.env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols, X: cols * 8, Y: rows * 16})
	if err != nil {
		return nil, err
	}
	defer ptmx.Close()

	// The reader goroutine owns nothing but its buffer; the terminal is only
	// touched on this goroutine.
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	var out bytes.Buffer
	consume := func(chunk []byte) {
		out.Write(chunk)
		term.VTWrite(chunk)
		for _, r := range replies {
			ptmx.Write(r)
		}
		replies = nil
	}
	screen := func() string {
		f, err := libghostty.NewFormatter(term, libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain))
		if err != nil {
			return ""
		}
		defer f.Close()
		s, _ := f.FormatString()
		return s
	}
	// pump consumes output until cond holds or the deadline passes.
	pump := func(deadline time.Time, cond func() bool) bool {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for !cond() {
			if time.Now().After(deadline) {
				return false
			}
			select {
			case c, ok := <-chunks:
				if !ok {
					// EOF: keep waiting on the ticker for the
					// program to be reaped.
					chunks = nil
					continue
				}
				consume(c)
			case <-tick.C:
			}
		}
		return true
	}

	for _, s := range sc.steps {
		if s.want != "" {
			wants := strings.Split(s.want, "|")
			ok := pump(time.Now().Add(30*time.Second), func() bool {
				text := screen()
				for _, w := range wants {
					if strings.Contains(text, w) {
						return true
					}
				}
				return false
			})
			if !ok {
				return nil, fmt.Errorf("timed out waiting for %q; screen:\n%s", s.want, screen())
			}
		}
		if s.only != "" && !strings.Contains(screen(), s.only) {
			continue
		}
		if s.send != "" {
			// Type like a person would, a key at a time, so programs
			// redraw per keystroke.
			for _, k := range splitKeys(s.send) {
				if _, err := ptmx.Write([]byte(k)); err != nil {
					return nil, err
				}
				pump(time.Now().Add(15*time.Millisecond), func() bool { return false })
			}
		}
		pump(time.Now().Add(s.pause), func() bool { return false })
		if verbose {
			fmt.Fprintf(os.Stderr, "--- after %q:\n%s\n", s.send, screen())
		}
	}

	// Let the program exit on its own, then drain the PTY.
	if !pump(time.Now().Add(60*time.Second), func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		return nil, errors.New("program did not exit; screen:\n" + screen())
	}
	pump(time.Now().Add(500*time.Millisecond), func() bool { return false })
	return out.Bytes(), nil
}

// splitKeys splits scripted input into keystrokes, keeping escape
// sequences such as arrow keys whole.
func splitKeys(s string) []string {
	var keys []string
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+2 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			keys = append(keys, s[i:j+1])
			i = j + 1
			continue
		}
		keys = append(keys, s[i:i+1])
		i++
	}
	return keys
}

// identifyingStrings lists what must not appear in a committed stream: the
// user's name and home directory, the host name, and git identity.
func identifyingStrings(extra []string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) >= 3 {
			out = append(out, s)
		}
	}
	if u, err := user.Current(); err == nil {
		add(u.HomeDir)
		add(u.Username)
		add(u.Name)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
		add(strings.Split(h, ".")[0])
	}
	for _, key := range []string{"user.email", "user.name"} {
		if v, err := exec.Command("git", "config", "--global", key).Output(); err == nil {
			add(string(v))
		}
	}
	for _, e := range extra {
		add(e)
	}
	return out
}

// scrub replaces each identifying string with a placeholder of the same
// length, so cursor positions and line wrapping are unchanged.
func scrub(data []byte, secrets []string) []byte {
	for _, s := range secrets {
		placeholder := []byte(s)
		for i, c := range placeholder {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
				placeholder[i] = 'x'
			case c >= '0' && c <= '9':
				placeholder[i] = '0'
			}
		}
		data = replaceFold(data, []byte(s), placeholder)
	}
	// Session ids and the like are not identifying, but they are not
	// worth keeping either.
	return uuidPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		return []byte("00000000-0000-0000-0000-000000000000")
	})
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func replaceFold(data, old, repl []byte) []byte {
	lower := asciiLower(data)
	needle := asciiLower(old)
	for i := 0; ; {
		j := bytes.Index(lower[i:], needle)
		if j < 0 {
			return data
		}
		copy(data[i+j:], repl)
		i += j + len(old)
	}
}

// asciiLower lowercases ASCII letters only, so offsets stay byte-for-byte
// the same as the input's.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func writeGzip(path string, data []byte) error {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	// No name or timestamp in the header, so re-recording identical output
	// gives an identical file.
	if _, err := io.Copy(zw, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
