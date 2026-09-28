package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultRemotePort is the UDP port `agentd remote enable` listens on when
// the address does not name one.
const DefaultRemotePort = "7433"

// AddressKind says what kind of network an address is on, which decides
// whether `agentd remote enable` may use a detected address without asking.
type AddressKind int

const (
	// KindTailscale is a Tailscale address: reachable only by the owner's
	// tailnet, so it is used without asking.
	KindTailscale AddressKind = iota
	// KindPrivate is a LAN address: reachable only from the same network,
	// and liable to change with DHCP.
	KindPrivate
	// KindPublic is reachable from the internet.
	KindPublic
)

func (k AddressKind) String() string {
	switch k {
	case KindTailscale:
		return "Tailscale"
	case KindPrivate:
		return "private LAN"
	default:
		return "public"
	}
}

// Tailscale assigns each machine an address in the CGNAT range and one in
// its own IPv6 ULA prefix.
var (
	tailscaleV4 = mustCIDR("100.64.0.0/10")
	tailscaleV6 = mustCIDR("fd7a:115c:a1e0::/48")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// ListenCandidate is an address `agentd remote enable` could listen on.
type ListenCandidate struct {
	IP   net.IP
	Kind AddressKind
}

// DetectListenAddress picks the address remote clients are most likely to
// reach this machine on: its Tailscale address if it has one, otherwise the
// address of the interface holding the default route. The caller decides
// whether a non-Tailscale address needs confirming.
func DetectListenAddress() (ListenCandidate, error) {
	if ip := interfaceTailscaleAddress(); ip != nil {
		return ListenCandidate{IP: ip, Kind: KindTailscale}, nil
	}
	if ip := cliTailscaleAddress(); ip != nil {
		return ListenCandidate{IP: ip, Kind: KindTailscale}, nil
	}
	ip, err := defaultRouteAddress()
	if err != nil {
		return ListenCandidate{}, err
	}
	return ListenCandidate{IP: ip, Kind: classifyAddress(ip)}, nil
}

// interfaceTailscaleAddress finds a Tailscale address on this machine's
// interfaces, preferring IPv4.
func interfaceTailscaleAddress() net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addrs []net.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if a, err := iface.Addrs(); err == nil {
			addrs = append(addrs, a...)
		}
	}
	return pickTailscale(addrs)
}

func pickTailscale(addrs []net.Addr) net.IP {
	var v6 net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		switch {
		case tailscaleV4.Contains(ipnet.IP):
			return ipnet.IP.To4()
		case v6 == nil && tailscaleV6.Contains(ipnet.IP):
			v6 = ipnet.IP
		}
	}
	return v6
}

// cliTailscaleAddress asks the tailscale CLI, which also covers userspace
// networking, where no interface carries the address.
func cliTailscaleAddress() net.IP {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "ip", "-4").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Fields(string(out)) {
		if ip := net.ParseIP(line); ip != nil && tailscaleV4.Contains(ip) {
			return ip.To4()
		}
	}
	return nil
}

// defaultRouteAddress is the source address the kernel would use to reach
// the internet. Connecting a UDP socket sends nothing; it only consults the
// routing table.
func defaultRouteAddress() (net.IP, error) {
	conn, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET-1: never contacted
	if err != nil {
		return nil, fmt.Errorf("no default route: %w", err)
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	if ip.IsLoopback() || ip.IsUnspecified() {
		return nil, errors.New("no usable network address")
	}
	return ip, nil
}

func classifyAddress(ip net.IP) AddressKind {
	switch {
	case tailscaleV4.Contains(ip) || tailscaleV6.Contains(ip):
		return KindTailscale
	case ip.IsPrivate() || ip.IsLinkLocalUnicast():
		return KindPrivate
	default:
		return KindPublic
	}
}

// ListenAddress turns what the user typed (a host, or host:port) into a
// listen address, adding the default port.
func ListenAddress(s string) (string, error) {
	if host, port, err := net.SplitHostPort(s); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf("`%s` is not an address (expected HOST or HOST:PORT)", s)
		}
		return s, nil
	}
	host := strings.Trim(s, "[]")
	if host == "" || strings.ContainsAny(host, " /") {
		return "", fmt.Errorf("`%s` is not an address (expected HOST or HOST:PORT)", s)
	}
	return net.JoinHostPort(host, DefaultRemotePort), nil
}

// defaultConfigText is written when enabling remote access on a machine
// that has no config.toml yet. It matches the built-in defaults (and what
// the agent CLI writes), so creating the file changes nothing else.
const defaultConfigText = `default_agent = "claude"

[agents.claude]
command = "claude"
args = []

[agents.codex]
command = "codex"
args = []
`

var (
	tableHeader = regexp.MustCompile(`^\s*\[`)
	remoteTable = regexp.MustCompile(`^\s*\[\s*remote\s*\]\s*(#[^\n]*)?\s*$`)
	listenKey   = regexp.MustCompile(`^\s*listen\s*=`)
)

// SetRemoteListen sets `listen` under [remote] in the config file at path,
// or removes it when listen is empty. The rest of the file, comments
// included, is kept as it is. The result is parsed before it is written, and
// a file this cannot edit safely (say, one using an inline table) is left
// alone with an error.
func SetRemoteListen(path, listen string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if listen == "" {
			return nil
		}
		data = []byte(defaultConfigText)
	} else if err != nil {
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	updated := editRemoteListen(string(data), listen)
	cfg = Config{}
	if _, err := toml.Decode(updated, &cfg); err != nil || cfg.Remote.Listen != listen {
		return fmt.Errorf("could not update [remote] listen in %s automatically; edit it by hand", path)
	}
	if updated == string(data) {
		return nil
	}
	return writeFileAtomic(path, []byte(updated))
}

func editRemoteListen(text, listen string) string {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	entry := fmt.Sprintf("listen = %q\n", listen)
	inRemote, header := false, -1
	for i, line := range lines {
		switch {
		case remoteTable.MatchString(line):
			inRemote, header = true, i
		case tableHeader.MatchString(line):
			inRemote = false
		case inRemote && listenKey.MatchString(line):
			if listen == "" {
				lines = append(lines[:i], lines[i+1:]...)
			} else {
				lines[i] = entry
			}
			return strings.Join(lines, "")
		}
	}
	if listen == "" {
		return text
	}
	if header >= 0 {
		lines = append(lines[:header+1], append([]string{entry}, lines[header+1:]...)...)
		return strings.Join(lines, "")
	}
	out := strings.Join(lines, "")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	return out + "[remote]\n" + entry
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

// CheckListen reports whether addr can be bound for QUIC right now, so a
// typo fails at once instead of leaving remote access silently off.
// current is the address a running daemon already holds: the port being
// taken by it is fine, since the restart releases it.
func CheckListen(addr, current string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		return pc.Close()
	}
	if current != "" && errors.Is(err, syscall.EADDRINUSE) && samePort(addr, current) {
		return nil
	}
	return fmt.Errorf("cannot listen on %s: %w", addr, err)
}

func samePort(a, b string) bool {
	_, pa, errA := net.SplitHostPort(a)
	_, pb, errB := net.SplitHostPort(b)
	return errA == nil && errB == nil && pa == pb
}
