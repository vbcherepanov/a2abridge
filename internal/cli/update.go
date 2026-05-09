package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/buildinfo"
)

const defaultRepo = "vbcherepanov/a2abridge"

func init() {
	registerCommand(Command{
		Name:    "update",
		Summary: "Self-update to the latest GitHub release",
		Run:     RunUpdate,
	})
}

// RunUpdate downloads the latest release artefact for this OS/arch and
// atomically replaces the running binary. The previous binary is renamed
// to <exe>.bak.<ts> so the user can roll back manually.
func RunUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", envOr("A2A_REPO", defaultRepo), "GitHub repo in owner/name form")
	want := fs.String("version", "", "specific tag to install (default: latest)")
	check := fs.Bool("check", false, "only print whether a newer version is available")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge update [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Downloads the latest a2abridge release for the current OS/arch and replaces")
		fmt.Fprintln(stderr, "the running binary atomically. The previous binary is kept as <exe>.bak.<ts>.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	target := *want
	if target == "" {
		latest, err := resolveLatestTag(*repo)
		if err != nil {
			fmt.Fprintf(stderr, "update: resolve latest tag: %v\n", err)
			return 1
		}
		target = latest
	}
	if target == "" {
		fmt.Fprintln(stderr, "update: no release found")
		return 1
	}

	current := normalizeVersion(buildinfo.Version)
	requested := normalizeVersion(target)
	fmt.Fprintf(stdout, "current: %s\nlatest:  %s\n", buildinfo.Version, target)

	if current == requested {
		fmt.Fprintln(stdout, "already up to date")
		return 0
	}
	if *check {
		fmt.Fprintln(stdout, "update available — run `a2abridge update` without --check to install")
		return 0
	}

	asset, err := assetName(target)
	if err != nil {
		fmt.Fprintf(stderr, "update: %v\n", err)
		return 1
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", *repo, target, asset)
	fmt.Fprintf(stdout, "downloading %s\n", url)

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "update: locate own binary: %v\n", err)
		return 1
	}
	if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = real
	}

	if err := downloadAndReplace(url, exe); err != nil {
		fmt.Fprintf(stderr, "update: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "updated %s → %s\n", buildinfo.Version, target)
	return 0
}

// resolveLatestTag asks GitHub for the most recent release tag.
func resolveLatestTag(repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("github api %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var meta struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", err
	}
	return meta.TagName, nil
}

// assetName picks the right release artefact for the current platform.
func assetName(tag string) (string, error) {
	v := strings.TrimPrefix(tag, "v")
	arch := runtime.GOARCH
	switch runtime.GOOS {
	case "darwin", "linux":
		return fmt.Sprintf("a2abridge_%s_%s_%s.tar.gz", v, runtime.GOOS, arch), nil
	case "windows":
		return fmt.Sprintf("a2abridge_%s_windows_%s.zip", v, arch), nil
	default:
		return "", fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// downloadAndReplace fetches url, extracts the a2abridge binary from the
// archive and atomically replaces dst. The old binary is preserved at
// <dst>.bak.<ts> so a manual rollback is one rename away.
func downloadAndReplace(url, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var fresh []byte
	switch {
	case strings.HasSuffix(url, ".tar.gz"):
		fresh, err = extractTarGzMember(body, "a2abridge")
	case strings.HasSuffix(url, ".zip"):
		fresh, err = extractZipMember(body, "a2abridge.exe")
	default:
		return fmt.Errorf("unknown archive format for %s", url)
	}
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Write to a sibling path then rename. On Windows os.Rename can replace
	// a running executable as long as we first rename the running one out
	// of the way (FILE_SHARE_DELETE semantics). We do that universally —
	// it's harmless on POSIX too and gives us a free rollback artefact.
	stamp := time.Now().Format("20060102-150405")
	bak := dst + ".bak." + stamp
	if err := os.Rename(dst, bak); err != nil {
		return fmt.Errorf("backup current: %w", err)
	}
	if err := os.WriteFile(dst, fresh, 0o755); err != nil {
		// Try to restore the backup on failure so the user is not left
		// without any binary at all.
		_ = os.Rename(bak, dst)
		return fmt.Errorf("write new binary: %w", err)
	}
	return nil
}

func extractTarGzMember(blob []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) != name {
			continue
		}
		return io.ReadAll(tr)
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

func extractZipMember(blob []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

// normalizeVersion strips a leading "v" and any "-dev" suffix so that
// "v0.2.0", "0.2.0", and "0.2.0-dev" all compare on equal footing for
// the up-to-date check (which is intentionally permissive — we don't try
// to compute precedence between "0.2.0" and "0.2.0-dev").
func normalizeVersion(s string) string {
	s = strings.TrimPrefix(s, "v")
	if i := strings.Index(s, "-"); i > 0 {
		s = s[:i]
	}
	return s
}
