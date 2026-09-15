package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/vbcherepanov/a2abridge/v4/internal/buildinfo"
)

const defaultRepo = "vbcherepanov/a2abridge"

// Download safety limits: GitHub release archives for this project are a
// few MiB; anything bigger than these caps is rejected outright instead of
// being buffered into memory.
const (
	maxArchiveBytes   = 256 << 20 // compressed release archive
	maxBinaryBytes    = 512 << 20 // decompressed binary inside the archive
	maxChecksumsBytes = 1 << 20   // checksums.txt
)

func init() {
	registerCommand(Command{
		Name:    "update",
		Summary: "Self-update to the latest GitHub release",
		Run:     RunUpdate,
	})
}

// RunUpdate downloads the latest release artefact for this OS/arch,
// verifies its SHA256 against the release's checksums.txt and atomically
// replaces the running binary. The previous binary is renamed to
// <exe>.bak.<ts> so the user can roll back manually.
func RunUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", envOr("A2A_REPO", defaultRepo), "GitHub repo in owner/name form")
	want := fs.String("version", "", "specific tag to install (default: latest)")
	check := fs.Bool("check", false, "only check: exit 0 when up to date, 1 when an update is available")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge update [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Downloads the latest a2abridge release for the current OS/arch, verifies its")
		fmt.Fprintln(stderr, "SHA256 against the release's checksums.txt and replaces the running binary")
		fmt.Fprintln(stderr, "atomically. The previous binary is kept as <exe>.bak.<ts>.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "With --check nothing is installed; the exit code is the answer:")
		fmt.Fprintln(stderr, "  0 — already up to date")
		fmt.Fprintln(stderr, "  1 — a newer version is available")
		fmt.Fprintln(stderr, "  2 — usage error")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// A2A_REPO silently redirects where the new binary comes from — make
	// sure the user notices a non-default source before we execute it.
	if env := os.Getenv("A2A_REPO"); env != "" && env != defaultRepo {
		fmt.Fprintf(stderr,
			"WARNING: A2A_REPO=%s overrides the update source (default: %s).\n"+
				"         The downloaded binary will come from that repository — make sure you trust it.\n",
			env, defaultRepo)
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

	fmt.Fprintf(stdout, "current: %s\nlatest:  %s\n", buildinfo.Version, target)

	// Unparseable versions ("dev" builds without ldflags) are treated as
	// older than any release — the safe default is to allow the update.
	if cmp, comparable := compareVersions(buildinfo.Version, target); comparable {
		if cmp == 0 {
			fmt.Fprintln(stdout, "already up to date")
			return 0
		}
		if cmp > 0 {
			if *want != "" {
				fmt.Fprintf(stderr, "update: refusing to downgrade from %s to %s — install the older release manually if you really need it\n",
					buildinfo.Version, target)
				return 1
			}
			fmt.Fprintf(stdout, "current version %s is newer than the latest release %s — nothing to do\n",
				buildinfo.Version, target)
			return 0
		}
	}
	if *check {
		fmt.Fprintln(stdout, "update available — run `a2abridge update` without --check to install")
		return 1
	}

	asset, err := assetName(target)
	if err != nil {
		fmt.Fprintf(stderr, "update: %v\n", err)
		return 1
	}
	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s", *repo, target)
	fmt.Fprintf(stdout, "downloading %s/%s\n", baseURL, asset)

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "update: locate own binary: %v\n", err)
		return 1
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	if err := downloadAndReplace(baseURL, asset, exe); err != nil {
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
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

// downloadAndReplace fetches baseURL/asset plus baseURL/checksums.txt,
// verifies the archive's SHA256 against the published checksum, extracts
// the a2abridge binary and atomically replaces dst. The old binary is
// preserved at <dst>.bak.<ts> so a manual rollback is one rename away.
func downloadAndReplace(baseURL, asset, dst string) error {
	body, err := httpGetLimited(baseURL+"/"+asset, maxArchiveBytes)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}

	if err := verifyChecksum(baseURL, asset, body); err != nil {
		return err
	}

	var fresh []byte
	switch {
	case strings.HasSuffix(asset, ".tar.gz"):
		fresh, err = extractTarGzMember(body, "a2abridge")
	case strings.HasSuffix(asset, ".zip"):
		fresh, err = extractZipMember(body, "a2abridge.exe")
	default:
		return fmt.Errorf("unknown archive format for %s", asset)
	}
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Atomic swap with rollback. The running binary is renamed out of the
	// way first — required on Windows, where a running exe cannot be
	// overwritten but CAN be renamed (FILE_SHARE_DELETE semantics) — then
	// the new binary lands at a sibling path and is renamed into place.
	stamp := time.Now().Format("20060102-150405")
	bak := dst + ".bak." + stamp
	if err := os.Rename(dst, bak); err != nil {
		return fmt.Errorf("backup current: %w", err)
	}
	tmp := dst + ".new"
	//nolint:gosec // G306: the replacement binary must stay executable (0755).
	if err := os.WriteFile(tmp, fresh, 0o755); err != nil {
		// Restore the backup on failure so the user is not left without
		// any binary at all.
		_ = os.Rename(bak, dst)
		return fmt.Errorf("write new binary: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		_ = os.Rename(bak, dst)
		return fmt.Errorf("activate new binary: %w", err)
	}
	return nil
}

// verifyChecksum downloads the checksums.txt published next to the release
// artefacts and compares body's SHA256 against the line for asset. A
// missing checksum file or a digest mismatch aborts the update — there is
// deliberately no insecure fallback.
func verifyChecksum(baseURL, asset string, body []byte) error {
	sums, err := httpGetLimited(baseURL+"/checksums.txt", maxChecksumsBytes)
	if err != nil {
		return fmt.Errorf("download checksums.txt (refusing to install an unverified artefact): %w", err)
	}
	want, err := findChecksum(sums, asset)
	if err != nil {
		return err
	}
	got := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return fmt.Errorf("checksum mismatch for %s: got %x, want %s", asset, got, want)
	}
	return nil
}

// findChecksum parses sha256sum-format lines ("<hex>  <filename>") and
// returns the digest recorded for name. sha256sum may prefix the filename
// with '*' (binary mode) — both forms are accepted.
func findChecksum(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s in checksums.txt", name)
}

// httpGetLimited GETs url and returns at most limit bytes, erroring out
// when the body exceeds the limit instead of silently truncating.
func httpGetLimited(url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response larger than %d bytes", limit)
	}
	return body, nil
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
		return readAllLimited(tr, maxBinaryBytes)
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
		data, rerr := readAllLimited(rc, maxBinaryBytes)
		if cerr := rc.Close(); rerr == nil && cerr != nil {
			rerr = cerr
		}
		if rerr != nil {
			return nil, rerr
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

// readAllLimited drains r up to limit bytes and errors out beyond that —
// a guard against decompression bombs in release archives.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("decompressed member larger than %d bytes", limit)
	}
	return data, nil
}

// parsedVersion is the minimal semver-ish decomposition used by
// compareVersions: up to three numeric core components plus an optional
// prerelease tail ("dev", "rc1", ...).
type parsedVersion struct {
	nums [3]int
	pre  string
}

// parseVersion accepts "v0.2.1", "0.3.0-rc1", "1.2" and similar. Returns
// ok=false for strings without a numeric core (e.g. bare "dev").
func parseVersion(s string) (parsedVersion, bool) {
	var v parsedVersion
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.Index(s, "-"); i >= 0 {
		v.pre = s[i+1:]
		s = s[:i]
		if v.pre == "" {
			return v, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.nums[i] = n
	}
	return v, true
}

// compareVersions compares two version strings, returning -1/0/+1 like
// strings.Compare. comparable=false means at least one side has no numeric
// core (e.g. a "dev" build) and no ordering can be established. A release
// sorts above any prerelease of the same core ("0.3.0-dev" < "0.3.0");
// two prereleases of the same core are ordered lexically.
func compareVersions(a, b string) (cmp int, comparable bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range av.nums {
		if av.nums[i] != bv.nums[i] {
			if av.nums[i] < bv.nums[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case av.pre == bv.pre:
		return 0, true
	case av.pre == "":
		return 1, true
	case bv.pre == "":
		return -1, true
	case av.pre < bv.pre:
		return -1, true
	default:
		return 1, true
	}
}
