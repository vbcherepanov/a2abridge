package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FederationConfig holds the four pieces of state that turn a
// loopback-only bridge into a cross-machine peer:
//
//	A2A_TLS_CERT     — server/client cert (PEM)
//	A2A_TLS_KEY      — matching private key (PEM, ed25519 or any TLS-acceptable algo)
//	A2A_TRUST_ROOTS  — colon-separated PEM bundle paths to validate peer certs
//	A2A_PEER_ALLOW   — comma-separated list of allowed peer SANs/CN; empty = any cert
//
// All four are optional. If neither cert nor key is set, the bridge runs
// in plain HTTP loopback mode (the v1.0 default).
type FederationConfig struct {
	CertFile    string
	KeyFile     string
	TrustRoots  []string // file paths, joined as a CA bundle
	PeerAllowCN []string // empty = any cert that chains to TrustRoots is OK
}

// FromEnv reads the federation config from environment variables. Missing
// values are returned as zero — callers check Enabled() before consuming.
func FromEnv() FederationConfig {
	cfg := FederationConfig{
		CertFile: os.Getenv("A2A_TLS_CERT"),
		KeyFile:  os.Getenv("A2A_TLS_KEY"),
	}
	if roots := os.Getenv("A2A_TRUST_ROOTS"); roots != "" {
		cfg.TrustRoots = splitPaths(roots)
	}
	if allow := os.Getenv("A2A_PEER_ALLOW"); allow != "" {
		for _, c := range strings.Split(allow, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				cfg.PeerAllowCN = append(cfg.PeerAllowCN, c)
			}
		}
	}
	return cfg
}

// Enabled reports whether the bridge should switch to mTLS. We require at
// least cert+key — without those, peers can't authenticate us, and the
// loopback-default is safer than partial-TLS.
func (c FederationConfig) Enabled() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// ServerTLSConfig builds a *tls.Config suitable for serving the A2A HTTP
// endpoint with mutual auth. The peer must present a certificate that
// chains to one of TrustRoots, and (if PeerAllowCN is non-empty) whose CN
// or SAN matches the allow-list.
func (c FederationConfig) ServerTLSConfig() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, errors.New("federation not enabled (A2A_TLS_CERT/A2A_TLS_KEY unset)")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	pool, err := loadTrustPool(c.TrustRoots)
	if err != nil {
		return nil, fmt.Errorf("load trust roots: %w", err)
	}
	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	if len(c.PeerAllowCN) > 0 {
		allow := map[string]bool{}
		for _, n := range c.PeerAllowCN {
			allow[strings.ToLower(n)] = true
		}
		tc.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			cert := state.PeerCertificates[0]
			candidates := append([]string{strings.ToLower(cert.Subject.CommonName)}, cert.DNSNames...)
			for _, cand := range candidates {
				if allow[strings.ToLower(cand)] {
					return nil
				}
			}
			return fmt.Errorf("peer CN/SAN %v not in allow-list %v", candidates, c.PeerAllowCN)
		}
	}
	return tc, nil
}

// ClientTLSConfig is the *tls.Config the bridge uses when contacting peers.
// Symmetrical: we present our cert, validate theirs against trust roots.
func (c FederationConfig) ClientTLSConfig() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	pool, err := loadTrustPool(c.TrustRoots)
	if err != nil {
		return nil, fmt.Errorf("load trust roots: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// loadTrustPool reads each path as a PEM bundle and returns a *x509.CertPool.
// Empty paths list returns nil so the caller can decide between
// "no roots = system pool" or "no roots = error".
func loadTrustPool(paths []string) (*x509.CertPool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		if !pool.AppendCertsFromPEM(raw) {
			return nil, fmt.Errorf("no PEM certs in %s", p)
		}
	}
	return pool, nil
}

// splitPaths splits a colon-separated list (POSIX) or semicolon (Windows)
// into individual file paths. We accept either separator on either OS for
// portability.
func splitPaths(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GenerateEd25519Cert produces a self-signed ed25519 key pair and writes
// it to <dir>/cert.pem + key.pem. The certificate is valid for 10 years
// — bridges are run by individuals, not enterprises, so we trade rotation
// hygiene for "set up once, forget about it".
//
// Returns the resolved cert/key paths so callers can echo them in `a2abridge
// service install --federation` output.
func GenerateEd25519Cert(dir, commonName string) (certPath, keyPath string, err error) {
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("ed25519 gen: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"a2abridge"}},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              []string{commonName, "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return "", "", fmt.Errorf("sign: %w", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}
