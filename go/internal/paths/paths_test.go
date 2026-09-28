package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverRootPriority(t *testing.T) {
	cases := []struct {
		name                   string
		agentd, xdg, home, tmp string
		preferHome             bool
		want                   string
	}{
		{"agentd dir wins", "/custom", "/run/user/501", "/Users/t", "/var/tmp", true, "/custom"},
		{"xdg next", "", "/run/user/501", "/Users/t", "/var/tmp", true, "/run/user/501/agentd"},
		{"macos home", "", "", "/Users/t", "/var/tmp", true, "/Users/t/.agentd"},
		{"tmpdir uid", "", "", "/Users/t", "/var/tmp", false, "/var/tmp/agentd-501"},
		{"tmp fallback", "", "", "", "", true, "/tmp/agentd-501"},
	}
	for _, c := range cases {
		got, err := discoverRoot(c.agentd, c.xdg, c.home, c.tmp, 501, c.preferHome)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestDerivedPaths(t *testing.T) {
	p := FromRoot("/Users/t/.agentd")
	if p.Socket != "/Users/t/.agentd/agentd.sock" || p.SessionSocketPath("x") != "/Users/t/.agentd/sessions/x.sock" {
		t.Fatalf("unexpected paths: %+v", p)
	}
}

func TestEnsureLayoutIsPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := FromRoot(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.Root, p.LogsDir, p.SessionsDir} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, %v", dir, info.Mode(), err)
		}
	}
}

func TestEnsureLayoutRefusesForeignRoot(t *testing.T) {
	// /usr is owned by root; the test is meaningless when running as root.
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	err := FromRoot("/usr").EnsureLayout()
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("got %v", err)
	}
}
