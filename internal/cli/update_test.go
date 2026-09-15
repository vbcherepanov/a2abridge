package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		cmp        int
		comparable bool
	}{
		{"v0.2.0", "v0.2.0", 0, true},
		{"0.2.0", "v0.2.0", 0, true},
		{"v0.2.0", "v0.3.0", -1, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.2", "v0.2.0", 0, true},
		{"0.3.0-dev", "v0.3.0", -1, true}, // prerelease < release
		{"v0.3.0", "0.3.0-rc1", 1, true},
		{"0.3.0-rc1", "0.3.0-rc2", -1, true},
		{"4.0.0+dirty", "v4.0.0", 0, true},                          // build metadata ignored
		{"4.0.1-0.20260915120000-abcdef123456", "v4.0.1", -1, true}, // pseudo-version < its release
		{"4.0.1-0.20260915120000-abcdef123456", "v4.0.0", 1, true},
		{"dev", "v0.3.0", 0, false},
		{"v0.3.0", "garbage", 0, false},
		{"", "v0.3.0", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if got != c.cmp || ok != c.comparable {
			t.Errorf("compareVersions(%q, %q) = (%d, %v), want (%d, %v)",
				c.a, c.b, got, ok, c.cmp, c.comparable)
		}
	}
}

func TestFindChecksum(t *testing.T) {
	sums := []byte(
		"abc123  a2abridge_0.3.0_darwin_arm64.tar.gz\n" +
			"def456 *a2abridge_0.3.0_windows_amd64.zip\n" +
			"malformed line without two fields and more\n")
	if got, err := findChecksum(sums, "a2abridge_0.3.0_darwin_arm64.tar.gz"); err != nil || got != "abc123" {
		t.Errorf("plain entry: got (%q, %v)", got, err)
	}
	if got, err := findChecksum(sums, "a2abridge_0.3.0_windows_amd64.zip"); err != nil || got != "def456" {
		t.Errorf("binary-mode entry: got (%q, %v)", got, err)
	}
	if _, err := findChecksum(sums, "missing.tar.gz"); err == nil {
		t.Error("missing entry: expected error")
	}
}

// buildTestArchive produces a tar.gz with a single "a2abridge" member.
func buildTestArchive(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "a2abridge", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadAndReplaceVerifiesChecksum(t *testing.T) {
	const asset = "a2abridge_0.0.1_test_amd64.tar.gz"
	newBinary := []byte("#!/bin/sh\necho new\n")
	archive := buildTestArchive(t, newBinary)
	sum := sha256.Sum256(archive)

	newDst := func(t *testing.T) string {
		dst := filepath.Join(t.TempDir(), "a2abridge")
		if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	serve := func(checksums string, withChecksums bool) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/"+asset, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(archive)
		})
		if withChecksums {
			mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, checksums)
			})
		}
		return httptest.NewServer(mux)
	}

	t.Run("valid checksum installs", func(t *testing.T) {
		srv := serve(fmt.Sprintf("%x  %s\n", sum, asset), true)
		defer srv.Close()
		dst := newDst(t)
		if err := downloadAndReplace(srv.URL, asset, dst); err != nil {
			t.Fatalf("downloadAndReplace: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, newBinary) {
			t.Errorf("dst content = %q, want new binary", got)
		}
		baks, _ := filepath.Glob(dst + ".bak.*")
		if len(baks) != 1 {
			t.Fatalf("expected exactly one .bak rollback file, got %v", baks)
		}
		old, _ := os.ReadFile(baks[0])
		if string(old) != "old-binary" {
			t.Errorf("backup content = %q, want old binary", old)
		}
	})

	t.Run("checksum mismatch aborts", func(t *testing.T) {
		srv := serve(fmt.Sprintf("%064d  %s\n", 0, asset), true)
		defer srv.Close()
		dst := newDst(t)
		err := downloadAndReplace(srv.URL, asset, dst)
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("expected checksum mismatch error, got %v", err)
		}
		got, _ := os.ReadFile(dst)
		if string(got) != "old-binary" {
			t.Errorf("dst was modified despite checksum mismatch: %q", got)
		}
	})

	t.Run("missing checksums.txt aborts", func(t *testing.T) {
		srv := serve("", false)
		defer srv.Close()
		dst := newDst(t)
		err := downloadAndReplace(srv.URL, asset, dst)
		if err == nil || !strings.Contains(err.Error(), "checksums.txt") {
			t.Fatalf("expected checksums.txt error, got %v", err)
		}
		got, _ := os.ReadFile(dst)
		if string(got) != "old-binary" {
			t.Errorf("dst was modified despite missing checksums: %q", got)
		}
	})

	t.Run("asset absent from checksums aborts", func(t *testing.T) {
		srv := serve(fmt.Sprintf("%x  some-other-asset.tar.gz\n", sum), true)
		defer srv.Close()
		dst := newDst(t)
		err := downloadAndReplace(srv.URL, asset, dst)
		if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
			t.Fatalf("expected missing-entry error, got %v", err)
		}
	})
}
