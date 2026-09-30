package transport

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/robmorgan/agentd/go/internal/session"
)

// Host is a remote daemon this machine can reach, pinned to its key.
//
// hosts.toml in the runtime root is the client side of agentd's SSH-style
// trust: like ~/.ssh/known_hosts, it records which key each named host must
// present. The daemon side (which client keys a daemon accepts) is the
// authorized-clients file.
type Host struct {
	Name string `toml:"-"`
	// Address is the UDP host:port of the daemon's QUIC listener.
	Address string `toml:"address"`
	// Fingerprint is the key the daemon must present.
	Fingerprint string `toml:"fingerprint"`
}

// ValidHostName checks a host name. Host names are used in host/session
// addresses, so they follow the session-name rules and never contain '/'.
func ValidHostName(name string) error {
	if !session.ValidName(name) {
		return fmt.Errorf("invalid host name `%s`: %s", name, session.NameRules)
	}
	return nil
}

// ReadHosts reads hosts.toml in file order. A missing file lists no hosts.
func ReadHosts(path string) ([]Host, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var file struct {
		Hosts map[string]Host `toml:"hosts"`
	}
	meta, err := toml.Decode(string(data), &file)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	var hosts []Host
	for _, key := range meta.Keys() {
		if len(key) == 2 && key[0] == "hosts" {
			host := file.Hosts[key[1]]
			host.Name = key[1]
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}

// LookupHost returns the named host from hosts.toml.
func LookupHost(path, name string) (Host, error) {
	hosts, err := ReadHosts(path)
	if err != nil {
		return Host{}, err
	}
	for _, host := range hosts {
		if host.Name == name {
			return host, nil
		}
	}
	return Host{}, fmt.Errorf("unknown host `%s`; add it with `agent host add %s ADDRESS`", name, name)
}

// AddHost adds a host to hosts.toml. A name that is already there is an
// error: a changed key must be replaced deliberately.
func AddHost(path string, host Host) error {
	if err := ValidHostName(host.Name); err != nil {
		return err
	}
	if !ValidFingerprint(host.Fingerprint) {
		return fmt.Errorf("`%s` is not a key fingerprint (expected SHA256:...)", host.Fingerprint)
	}
	return editHosts(path, func(hosts []Host) ([]Host, error) {
		for _, h := range hosts {
			if h.Name == host.Name {
				return nil, fmt.Errorf("host `%s` already exists; remove it first with `agent host rm %s`", host.Name, host.Name)
			}
		}
		return append(hosts, host), nil
	})
}

// RemoveHost removes a host from hosts.toml and reports whether it was
// there.
func RemoveHost(path, name string) (bool, error) {
	removed := false
	err := editHosts(path, func(hosts []Host) ([]Host, error) {
		kept := hosts[:0:0]
		for _, h := range hosts {
			if h.Name == name {
				removed = true
				continue
			}
			kept = append(kept, h)
		}
		if !removed {
			return nil, nil
		}
		return kept, nil
	})
	return removed, err
}

// editHosts holds the hosts.toml lock across the read, change and write, so
// concurrent `agent host` commands never lose each other's changes, and
// replaces the file atomically. edit returning nil hosts and no error means
// nothing changed.
func editHosts(path string, edit func([]Host) ([]Host, error)) error {
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	hosts, err := ReadHosts(path)
	if err != nil {
		return err
	}
	updated, err := edit(hosts)
	if err != nil || updated == nil {
		return err
	}
	var out strings.Builder
	for i, host := range updated {
		if i > 0 {
			out.WriteString("\n")
		}
		// Valid host names are bare TOML keys.
		fmt.Fprintf(&out, "[hosts.%s]\n", host.Name)
		values, err := toml.Marshal(host)
		if err != nil {
			return err
		}
		out.Write(values)
	}
	return replaceFile(path, []byte(out.String()))
}
