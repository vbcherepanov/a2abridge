//go:build integration

// Builds the real a2abridge command the way a consumer does — from another
// module that requires this one — and checks the version it prints:
//
//	go test -tags=integration ./internal/buildinfo/
package buildinfo_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	moduleUnderTest  = "github.com/vbcherepanov/a2abridge/v4"
	requiredVersion  = "v4.0.1"
	releaseLDVersion = "9.9.9-release"
)

// buildFromConsumer builds cmd/a2abridge inside a throwaway module that
// requires moduleUnderTest at requiredVersion, replaced by this checkout.
func buildFromConsumer(t *testing.T, ldflags string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	goMod := fmt.Sprintf("module example.com/installcheck\n\ngo 1.25.5\n\nrequire %s %s\n\nreplace %s => %s\n",
		moduleUnderTest, requiredVersion, moduleUnderTest, filepath.ToSlash(root))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sums, 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "a2abridge")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"build", "-o", bin}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, moduleUnderTest+"/cmd/a2abridge")
	build := exec.Command("go", args...)
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build from consumer module: %v\n%s", err, out)
	}
	return bin
}

func versionLine(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", bin, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}

func TestConsumerBuildReportsModuleVersion(t *testing.T) {
	bin := buildFromConsumer(t, "")
	if got, want := versionLine(t, bin), "a2abridge "+strings.TrimPrefix(requiredVersion, "v"); got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestReleaseLDFlagsWin(t *testing.T) {
	bin := buildFromConsumer(t, "-X "+moduleUnderTest+"/internal/buildinfo.Version="+releaseLDVersion)
	if got, want := versionLine(t, bin), "a2abridge "+releaseLDVersion; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}
