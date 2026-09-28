package transport

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is a daemon's or a client's key for remote connections. Peers
// recognise each other by the key's fingerprint, the way SSH uses host keys
// and authorized_keys: there is no certificate authority. The certificate
// is regenerated from the key whenever the identity is loaded; only the key
// is persisted.
type Identity struct {
	cert        tls.Certificate
	Fingerprint string
}

const fingerprintPrefix = "SHA256:"

// Fingerprint identifies a peer by its public key: "SHA256:" and the
// unpadded base64 SHA-256 of the certificate's SubjectPublicKeyInfo. It does
// not change when the certificate is regenerated from the same key.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return fingerprintPrefix + base64.RawStdEncoding.EncodeToString(sum[:])
}

// ValidFingerprint reports whether s is a well-formed fingerprint.
func ValidFingerprint(s string) bool {
	raw, ok := strings.CutPrefix(s, fingerprintPrefix)
	if !ok {
		return false
	}
	sum, err := base64.RawStdEncoding.DecodeString(raw)
	return err == nil && len(sum) == sha256.Size
}

// GenerateIdentity creates a new in-memory identity.
func GenerateIdentity() (*Identity, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return identityFromKey(key)
}

// LoadOrCreateIdentity loads the key at path, creating it (0600, in a 0700
// directory) if it does not exist yet.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createIdentity(path)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return identityFromKey(key)
}

func createIdentity(path string) (*Identity, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	// Write to a temporary file and rename, so a crash never leaves a
	// truncated key that would change the identity on the next start.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := pem.Encode(tmp, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return identityFromKey(key)
}

func identityFromKey(key ed25519.PrivateKey) (*Identity, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "agentd"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{
		cert:        tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		Fingerprint: Fingerprint(leaf),
	}, nil
}

// AuthorizedClient is one entry of an authorized-clients file.
type AuthorizedClient struct {
	Fingerprint string
	Name        string
}

// ReadAuthorized reads an authorized-clients file: one fingerprint per line,
// optionally followed by a name; blank lines and lines starting with # are
// ignored. A missing file authorizes nobody.
func ReadAuthorized(path string) ([]AuthorizedClient, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuthorizedClient
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fp, name, _ := strings.Cut(line, " ")
		if !ValidFingerprint(fp) {
			continue
		}
		out = append(out, AuthorizedClient{Fingerprint: fp, Name: strings.TrimSpace(name)})
	}
	return out, scanner.Err()
}

// IsAuthorized reports whether fp is listed in the authorized-clients file.
func IsAuthorized(path, fp string) (bool, error) {
	clients, err := ReadAuthorized(path)
	if err != nil {
		return false, err
	}
	for _, c := range clients {
		if c.Fingerprint == fp {
			return true, nil
		}
	}
	return false, nil
}

// Authorize adds a client to the authorized-clients file. It reports false
// if the fingerprint was already listed.
func Authorize(path, fp, name string) (bool, error) {
	if !ValidFingerprint(fp) {
		return false, fmt.Errorf("%q is not a key fingerprint (expected SHA256:...)", fp)
	}
	if ok, err := IsAuthorized(path, fp); err != nil || ok {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	line := fp
	if name = strings.TrimSpace(name); name != "" {
		line += " " + name
	}
	_, err = fmt.Fprintln(f, line)
	return err == nil, err
}

// Revoke removes a client from the authorized-clients file. It reports
// whether the fingerprint was listed.
func Revoke(path, fp string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var kept []string
	removed := false
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if first, _, _ := strings.Cut(strings.TrimSpace(line), " "); first == fp {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return true, os.WriteFile(path, []byte(out), 0o600)
}
