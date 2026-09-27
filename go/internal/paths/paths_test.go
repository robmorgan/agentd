package paths

import "testing"

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
