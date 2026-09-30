package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robmorgan/agentd/go/internal/config"
)

func TestEditRemoteListen(t *testing.T) {
	const addr = "100.64.0.5:7433"
	for _, tc := range []struct{ name, in, want string }{
		{"no table", "default_agent = \"sh\"\n", "default_agent = \"sh\"\n\n[remote]\nlisten = \"100.64.0.5:7433\"\n"},
		{"no trailing newline", "default_agent = \"sh\"", "default_agent = \"sh\"\n\n[remote]\nlisten = \"100.64.0.5:7433\"\n"},
		{"empty table", "[remote]\n# where to listen\n[agents.sh]\ncommand = \"sh\"\n",
			"[remote]\nlisten = \"100.64.0.5:7433\"\n# where to listen\n[agents.sh]\ncommand = \"sh\"\n"},
		{"replace", "# mine\n[remote] # remote access\nlisten = \"1.2.3.4:1\" # old\n\n[agents.sh]\n",
			"# mine\n[remote] # remote access\nlisten = \"100.64.0.5:7433\"\n\n[agents.sh]\n"},
		{"listen elsewhere is not touched", "[agents.x]\nlisten = 1\n", "[agents.x]\nlisten = 1\n\n[remote]\nlisten = \"100.64.0.5:7433\"\n"},
	} {
		if got := editRemoteListen(tc.in, addr); got != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
	if got := editRemoteListen("[remote]\nlisten = \"a:1\"\n# kept\n", ""); got != "[remote]\n# kept\n" {
		t.Errorf("remove: %q", got)
	}
	if got := editRemoteListen("default_agent = \"sh\"\n", ""); got != "default_agent = \"sh\"\n" {
		t.Errorf("remove when absent: %q", got)
	}
	if got := editRemoteListen("x = 1\n[remote]", addr); got != "x = 1\n[remote]\nlisten = \"100.64.0.5:7433\"\n" {
		t.Errorf("[remote] as the last line: %q", got)
	}
}

func TestSetRemoteListen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// No config yet: the defaults are written along with [remote].
	if err := SetRemoteListen(path, "100.64.0.5:7433"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Remote.Listen != "100.64.0.5:7433" || cfg.DefaultAgent != "claude" || cfg.Agents["codex"].Command != "codex" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode())
	}
	if err := SetRemoteListen(path, ""); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.Load(path); cfg.Remote.Listen != "" {
		t.Fatalf("listen after disable = %q", cfg.Remote.Listen)
	}

	// Layouts the line editor does not handle are refused, not mangled,
	// including lines that only look like [remote] inside a string.
	for _, text := range []string{
		"remote = { listen = \"a:1\" }\n",
		"remote.listen = \"a:1\"\n",
		"not toml [\n",
		"[agents.x]\ncommand = \"x\"\nargs = [\"\"\"\n[remote]\nlisten = \"a:1\"\n\"\"\"]\n",
	} {
		os.WriteFile(path, []byte(text), 0o600)
		if err := SetRemoteListen(path, "b:2"); err == nil {
			t.Errorf("edited %q", text)
		}
		if data, _ := os.ReadFile(path); string(data) != text {
			t.Errorf("file changed to %q", data)
		}
	}
}

func TestListenAddress(t *testing.T) {
	for in, want := range map[string]string{
		"100.64.0.5":      "100.64.0.5:7433",
		"100.64.0.5:9000": "100.64.0.5:9000",
		"devbox.ts.net":   "devbox.ts.net:7433",
		"fd7a::1":         "[fd7a::1]:7433",
		"[fd7a::1]:9000":  "[fd7a::1]:9000",
	} {
		if got, err := ListenAddress(in); err != nil || got != want {
			t.Errorf("ListenAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ":7433", "a b", "host:"} {
		if got, err := ListenAddress(bad); err == nil {
			t.Errorf("ListenAddress(%q) = %q", bad, got)
		}
	}
}

func TestAddressDetectionHelpers(t *testing.T) {
	addr := func(iface, ip string) interfaceAddress { return interfaceAddress{iface: iface, ip: net.ParseIP(ip)} }
	lan, ts6, ts4 := addr("en0", "192.168.1.20"), addr("tailscale0", "fd7a:115c:a1e0::5"), addr("tailscale0", "100.101.102.103")
	if ip := pickTailscale([]interfaceAddress{lan, ts6, ts4}, nil); !ip.Equal(net.ParseIP("100.101.102.103")) {
		t.Fatalf("picked %v, want the IPv4 Tailscale address", ip)
	}
	if ip := pickTailscale([]interfaceAddress{lan, ts6}, nil); !ip.Equal(net.ParseIP("fd7a:115c:a1e0::5")) {
		t.Fatalf("picked %v, want the IPv6 Tailscale address", ip)
	}
	if ip := pickTailscale([]interfaceAddress{lan}, nil); ip != nil {
		t.Fatalf("picked %v from a LAN-only machine", ip)
	}
	// A CGNAT address from a carrier or another VPN is not Tailscale's...
	carrier := addr("rmnet0", "100.72.1.9")
	if ip := pickTailscale([]interfaceAddress{carrier}, nil); ip != nil {
		t.Fatalf("picked unconfirmed %v", ip)
	}
	// ...unless the tailscale CLI vouches for it (macOS names it utunN).
	mac := addr("utun4", "100.115.231.17")
	if ip := pickTailscale([]interfaceAddress{carrier, mac}, []net.IP{net.ParseIP("100.115.231.17")}); !ip.Equal(mac.ip) {
		t.Fatalf("picked %v, want the CLI-confirmed %v", ip, mac.ip)
	}
	// An address the CLI reports but no interface holds cannot be bound.
	if ip := pickTailscale([]interfaceAddress{lan}, []net.IP{net.ParseIP("100.64.0.9")}); ip != nil {
		t.Fatalf("picked %v, which no interface holds", ip)
	}
	for s, want := range map[string]AddressKind{
		"100.64.0.1": KindShared, "192.168.1.20": KindPrivate, "10.0.0.3": KindPrivate,
		"8.8.8.8": KindPublic, "2001:4860::1": KindPublic, "fd00::1": KindPrivate,
	} {
		if got := classifyAddress(net.ParseIP(s)); got != want {
			t.Errorf("classify %s = %v, want %v", s, got, want)
		}
	}
}

func TestCheckListen(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	busy := pc.LocalAddr().String()
	if err := CheckListen(busy, ""); err == nil {
		t.Fatal("a port in use passed")
	}
	// The running daemon holding it is fine, including when the address
	// changes but the port stays (the restart releases the port).
	if err := CheckListen(busy, busy); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(busy)
	if err := CheckListen(net.JoinHostPort("0.0.0.0", port), busy); err != nil {
		t.Fatal(err)
	}
	if err := CheckListen("127.0.0.1:0", ""); err != nil {
		t.Fatal(err)
	}
}

// A daemon started before its listen address exists (Tailscale still
// coming up) starts listening once it can.
func TestRemoteListenerRetriesUntilItCanBind(t *testing.T) {
	// Restored by a cleanup registered before the harness's, so it runs
	// after the daemon (and its retry goroutine) has stopped.
	old := remoteRetryInterval
	remoteRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() { remoteRetryInterval = old })

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := pc.LocalAddr().String()
	h := newHarnessWithConfig(t, "\n[remote]\nlisten = \""+busy+"\"\n")

	status, err := managementStatus(h.paths)
	if err != nil || status.Remote != "" || status.RemoteError == "" {
		pc.Close()
		t.Fatalf("status while blocked = %+v, %v", status, err)
	}
	pc.Close()
	h.eventually("remote listener", func() bool { return h.srv.RemoteAddr() == busy })
	status, err = managementStatus(h.paths)
	if err != nil || status.Remote != busy || status.RemoteError != "" {
		t.Fatalf("status once bound = %+v, %v", status, err)
	}
}

func TestDefaultRouteAddress(t *testing.T) {
	ip, err := defaultRouteAddress()
	if err != nil {
		t.Skipf("no default route here: %v", err)
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		t.Fatalf("default route address %v", ip)
	}
}

// A symlinked config (say, from a dotfiles repository) stays a symlink, and
// the file keeps its mode.
func TestSetRemoteListenFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-config.toml")
	if err := os.WriteFile(real, []byte("default_agent = \"sh\"\n[agents.sh]\ncommand = \"sh\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := SetRemoteListen(link, "100.64.0.5:7433"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.toml is no longer a symlink: %v %v", info.Mode(), err)
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
	if cfg, err := config.Load(link); err != nil || cfg.Remote.Listen != "100.64.0.5:7433" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}
