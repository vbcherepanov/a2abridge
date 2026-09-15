package buildinfo

import (
	"runtime/debug"
	"testing"
)

const (
	testRevision = "62f5be5a1b2c3d4e5f60718293a4b5c6d7e8f901"
	testVCSTime  = "2026-09-15T11:26:21Z"
)

func vcs(modified bool) []debug.BuildSetting {
	m := "false"
	if modified {
		m = "true"
	}
	return []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: testRevision},
		{Key: "vcs.time", Value: testVCSTime},
		{Key: "vcs.modified", Value: m},
	}
}

func mainModule(version string, settings []debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Path:     modulePath + "/cmd/a2abridge",
		Main:     debug.Module{Path: modulePath, Version: version},
		Settings: settings,
	}
}

func TestModulePath(t *testing.T) {
	if modulePath != "github.com/vbcherepanov/a2abridge/v4" {
		t.Fatalf("modulePath = %q", modulePath)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name   string
		linked Info
		bi     *debug.BuildInfo
		want   Info
	}{
		{
			name:   "release ldflags win over module metadata",
			linked: Info{Version: "4.0.1", Commit: "abc1234", BuildDate: "2026-09-15T12:00:00Z"},
			bi:     mainModule("v4.0.0", vcs(true)),
			want:   Info{Version: "4.0.1", Commit: "abc1234", BuildDate: "2026-09-15T12:00:00Z"},
		},
		{
			name: "go install module@version",
			bi:   mainModule("v4.0.0", nil),
			want: Info{Version: "4.0.0", Commit: Unknown, BuildDate: Unknown},
		},
		{
			name: "pseudo-version reported as-is",
			bi:   mainModule("v4.0.1-0.20260915120000-abcdef123456", nil),
			want: Info{Version: "4.0.1-0.20260915120000-abcdef123456", Commit: Unknown, BuildDate: Unknown},
		},
		{
			name: "toolchain-stamped dirty pseudo-version",
			bi:   mainModule("v4.0.1-0.20260915112621-62f5be5a1b2c+dirty", vcs(true)),
			want: Info{Version: "4.0.1-0.20260915112621-62f5be5a1b2c+dirty", Commit: "62f5be5-dirty", BuildDate: testVCSTime},
		},
		{
			name: "local checkout build: devel with clean vcs stamp",
			bi:   mainModule("(devel)", vcs(false)),
			want: Info{Version: DevVersion, Commit: "62f5be5", BuildDate: testVCSTime},
		},
		{
			name: "local checkout build: modified tree",
			bi:   mainModule("(devel)", vcs(true)),
			want: Info{Version: DevVersion, Commit: "62f5be5-dirty", BuildDate: testVCSTime},
		},
		{
			name:   "partial ldflags keep vcs fallback for the rest",
			linked: Info{Version: "4.0.1"},
			bi:     mainModule("(devel)", vcs(false)),
			want:   Info{Version: "4.0.1", Commit: "62f5be5", BuildDate: testVCSTime},
		},
		{
			name: "built as a dependency of another module",
			bi: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/tools", Version: "(devel)"},
				Deps: []*debug.Module{
					{Path: "github.com/a2aproject/a2a-go/v2", Version: "v2.5.0"},
					{Path: modulePath, Version: "v4.2.0", Replace: &debug.Module{Path: "../a2abridge"}},
				},
			},
			want: Info{Version: "4.2.0", Commit: Unknown, BuildDate: Unknown},
		},
		{
			name: "empty module version",
			bi:   mainModule("", nil),
			want: Info{Version: DevVersion, Commit: Unknown, BuildDate: Unknown},
		},
		{
			name: "non-semver module version",
			bi:   mainModule("latest", nil),
			want: Info{Version: DevVersion, Commit: Unknown, BuildDate: Unknown},
		},
		{
			name: "no build info",
			want: Info{Version: DevVersion, Commit: Unknown, BuildDate: Unknown},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.linked, tc.bi); got != tc.want {
				t.Fatalf("resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGetResolvesOnce: the effective values are stable and never the stale
// hard-coded default the 4.0.0 binaries reported.
func TestGetResolvesOnce(t *testing.T) {
	first := Get()
	if first != Get() {
		t.Fatal("Get changed between calls")
	}
	if first.Version == "" || first.Version == "3.0.0-dev" || first.Commit == "" || first.BuildDate == "" {
		t.Fatalf("Get = %+v", first)
	}
}
