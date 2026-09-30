package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testFingerprint = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"

func TestHostsRoundTripAndArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.toml")
	if hosts, err := ReadHosts(path); err != nil || len(hosts) != 0 {
		t.Fatalf("missing file: %v %v", hosts, err)
	}
	host := Host{Name: "devbox", Address: "100.64.0.5:7433", Fingerprint: testFingerprint}
	if err := AddHost(path, host); err != nil {
		t.Fatal(err)
	}
	if err := AddHost(path, host); err == nil || !strings.Contains(err.Error(), "agent host rm devbox") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := AddHost(path, Host{Name: "Dev/Box", Address: host.Address, Fingerprint: testFingerprint}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if err := AddHost(path, Host{Name: "other", Address: host.Address, Fingerprint: "SHA256:nope"}); err == nil {
		t.Fatal("malformed fingerprint accepted")
	}
	if got, err := LookupHost(path, "devbox"); err != nil || got != host {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if removed, err := RemoveHost(path, "devbox"); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := RemoveHost(path, "devbox"); err != nil || removed {
		t.Fatalf("second remove: %v %v", removed, err)
	}
	if _, err := LookupHost(path, "devbox"); err == nil || !strings.Contains(err.Error(), "agent host add devbox") {
		t.Fatalf("lookup after remove: %v", err)
	}
}

func TestHostsKeepFileOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.toml")
	names := []string{"zeta", "alpha", "mid"}
	for _, name := range names {
		if err := AddHost(path, Host{Name: name, Address: name + ":7433", Fingerprint: testFingerprint}); err != nil {
			t.Fatal(err)
		}
	}
	hosts, err := ReadHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range hosts {
		if h.Name != names[i] || h.Address != names[i]+":7433" {
			t.Fatalf("hosts[%d] = %+v", i, h)
		}
	}
}

// Files written by the Rust CLI (toml::to_string_pretty) still read.
func TestHostsReadsExistingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.toml")
	os.WriteFile(path, []byte("[hosts.devbox]\naddress = \"100.64.0.5:7433\"\nfingerprint = \""+testFingerprint+"\"\n"), 0o600)
	if h, err := LookupHost(path, "devbox"); err != nil || h.Address != "100.64.0.5:7433" || h.Fingerprint != testFingerprint {
		t.Fatalf("got %+v %v", h, err)
	}
}

func TestConcurrentHostAddsAreAllKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.toml")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			digest := sha256.Sum256([]byte{byte(i)})
			fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
			if err := AddHost(path, Host{Name: fmt.Sprintf("host-%d", i), Address: fmt.Sprintf("10.0.0.%d:7433", i), Fingerprint: fp}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if hosts, err := ReadHosts(path); err != nil || len(hosts) != 8 {
		t.Fatalf("got %d hosts, %v", len(hosts), err)
	}
}
