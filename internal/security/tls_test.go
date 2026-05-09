package security

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateEd25519CertProducesValidPEM exercises the full key-gen
// path, including the certificate's intended dual server+client EKU.
func TestGenerateEd25519CertProducesValidPEM(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := GenerateEd25519Cert(dir, "test-peer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(certPath, dir) || !strings.HasPrefix(keyPath, dir) {
		t.Errorf("paths outside requested dir: %s, %s", certPath, keyPath)
	}

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("cert.pem missing CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "test-peer" {
		t.Errorf("CN = %q, want test-peer", cert.Subject.CommonName)
	}
	if !hasEKU(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Error("cert missing ServerAuth EKU")
	}
	if !hasEKU(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Error("cert missing ClientAuth EKU")
	}

	// Make sure tls can load the pair (proves cert+key match).
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
}

func hasEKU(usages []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, u := range usages {
		if u == want {
			return true
		}
	}
	return false
}

// TestServerTLSConfigEnforcesClientCert spins up a real HTTPS server with
// our ServerTLSConfig and verifies that:
//   - a client without a cert is rejected;
//   - a client with the matching cert is accepted.
func TestServerTLSConfigEnforcesClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := GenerateEd25519Cert(dir, "self-peer")
	if err != nil {
		t.Fatal(err)
	}

	cfg := FederationConfig{
		CertFile:   certPath,
		KeyFile:    keyPath,
		TrustRoots: []string{certPath}, // self-trust for round-trip
	}
	tlsCfg, err := cfg.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	// Without a cert: connection must fail.
	bareClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := bareClient.Get(srv.URL); err == nil {
		t.Error("anonymous TLS request unexpectedly succeeded")
	}

	// With our own cert: it must be accepted (server.TrustRoots includes it).
	clientCfg, err := cfg.ClientTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	authedClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := authedClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("authed request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestFederationFromEnvSeparators verifies the colon/semicolon-tolerant
// path splitter for A2A_TRUST_ROOTS.
func TestFederationFromEnvSeparators(t *testing.T) {
	cases := map[string][]string{
		"":           nil,
		"a":          {"a"},
		"a:b":        {"a", "b"},
		"a;b":        {"a", "b"},
		"a:b;c":      {"a", "b", "c"},
		" a : b ":    {"a", "b"},
	}
	for in, want := range cases {
		t.Setenv("A2A_TRUST_ROOTS", in)
		got := FromEnv().TrustRoots
		if len(got) != len(want) {
			t.Errorf("FromEnv(%q).TrustRoots = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("FromEnv(%q).TrustRoots[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

// TestPeerAllowListBlocksUnlistedCN verifies that the allow-list short-circuits
// connections from peers whose CN/SAN is not in the configured set.
func TestPeerAllowListBlocksUnlistedCN(t *testing.T) {
	dir := t.TempDir()
	srvCert, srvKey, _ := GenerateEd25519Cert(filepath.Join(dir, "srv"), "server")
	clientCert, clientKey, _ := GenerateEd25519Cert(filepath.Join(dir, "cli"), "rogue-client")

	cfg := FederationConfig{
		CertFile:    srvCert,
		KeyFile:     srvKey,
		TrustRoots:  []string{srvCert, clientCert},
		PeerAllowCN: []string{"trusted-only"}, // does NOT include "rogue-client"
	}
	tlsCfg, err := cfg.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	clientCfg := &tls.Config{
		Certificates: []tls.Certificate{
			loadCert(t, clientCert, clientKey),
		},
		RootCAs: tlsCfg.ClientCAs,
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	if _, err := client.Get(srv.URL); err == nil {
		t.Error("expected allow-list to reject rogue-client, got nil error")
	}
}

func loadCert(t *testing.T, certPath, keyPath string) tls.Certificate {
	t.Helper()
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
