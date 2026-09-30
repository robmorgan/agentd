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
		want                   string
	}{
		{"agentd dir wins", "/custom", "/run/user/501", "/home/t", "/var/tmp", "/custom"},
		// Home wins over XDG_RUNTIME_DIR: /run/user is wiped on reboot.
		{"home next", "", "/run/user/501", "/home/t", "/var/tmp", "/home/t/.agentd"},
		{"xdg without home", "", "/run/user/501", "", "/var/tmp", "/run/user/501/agentd"},
		{"tmpdir uid", "", "", "", "/var/tmp", "/var/tmp/agentd-501"},
		{"tmp fallback", "", "", "", "", "/tmp/agentd-501"},
	}
	for _, c := range cases {
		got, err := discoverRoot(c.agentd, c.xdg, c.home, c.tmp, 501)
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
	if p.ClientKeyPath() != "/Users/t/.agentd/remote/client.key" || p.HostsPath() != "/Users/t/.agentd/hosts.toml" {
		t.Fatalf("unexpected remote paths: %q %q", p.ClientKeyPath(), p.HostsPath())
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

func TestEnsureLayoutRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	os.Mkdir(target, 0o700)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := FromRoot(link).EnsureLayout()
	if err == nil || !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), link) {
		t.Fatalf("root: got %v", err)
	}

	// A subdirectory swapped for a symlink or a file is refused too.
	root := filepath.Join(dir, "root")
	p := FromRoot(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	os.Remove(p.LogsDir)
	os.Symlink(target, p.LogsDir)
	if err := p.EnsureLayout(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("logs symlink: got %v", err)
	}
	os.Remove(p.LogsDir)
	os.WriteFile(p.LogsDir, nil, 0o600)
	if err := p.EnsureLayout(); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("logs file: got %v", err)
	}
}

func TestEnsureLayoutTightensSubdirectories(t *testing.T) {
	p := FromRoot(filepath.Join(t.TempDir(), "root"))
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p.SessionsDir, 0o755)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p.SessionsDir); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", info.Mode())
	}
}
