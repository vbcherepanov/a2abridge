// Package buildinfo identifies the running build: version, commit and build date.
package buildinfo

import (
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
)

// Link-time overrides, set by release builds:
//
//	go build -ldflags "-X github.com/vbcherepanov/a2abridge/v4/internal/buildinfo.Version=4.0.1 \
//	  -X github.com/vbcherepanov/a2abridge/v4/internal/buildinfo.Commit=62f5be5 \
//	  -X github.com/vbcherepanov/a2abridge/v4/internal/buildinfo.BuildDate=2026-09-15T12:00:00Z"
//
// Empty means unset. Callers read the effective values through Get.
var (
	Version   string
	Commit    string
	BuildDate string
)

const (
	// DevVersion is reported when neither ldflags nor module metadata name a version.
	DevVersion = "dev"
	// Unknown is reported for a commit or build date that was not recorded.
	Unknown = "unknown"

	develModuleVersion = "(devel)"
	shortRevisionLen   = 7
	dirtySuffix        = "-dirty"
)

// Info is the effective identification of this build.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}

// semverPattern matches module versions: releases, prereleases,
// pseudo-versions, and build metadata such as +incompatible or +dirty.
var semverPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// modulePath is this module's path, derived from the package path so it
// follows the next major-version rename.
var modulePath = strings.TrimSuffix(reflect.TypeFor[Info]().PkgPath(), "/internal/buildinfo")

var current = sync.OnceValue(func() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return resolve(Info{Version: Version, Commit: Commit, BuildDate: BuildDate}, bi)
})

// Get returns the effective build identification, resolved once.
func Get() Info {
	return current()
}

// resolve fills each field by precedence: the link-time value, then what the
// Go toolchain recorded in bi (module version, VCS stamp), then a default.
func resolve(linked Info, bi *debug.BuildInfo) Info {
	info := linked
	if info.Version == "" {
		info.Version = moduleVersion(bi)
	}
	settings := buildSettings(bi)
	if info.Commit == "" {
		info.Commit = vcsCommit(settings)
	}
	if info.BuildDate == "" {
		info.BuildDate = settings["vcs.time"]
		if info.BuildDate == "" {
			info.BuildDate = Unknown
		}
	}
	return info
}

// moduleVersion returns this module's version without the "v" prefix. The
// module is the main module for `go install .../cmd/a2abridge@version` and a
// dependency when another module builds the command. A missing version,
// "(devel)" or anything that is not semver yields DevVersion.
func moduleVersion(bi *debug.BuildInfo) string {
	if bi == nil {
		return DevVersion
	}
	var mod *debug.Module
	if bi.Main.Path == modulePath {
		mod = &bi.Main
	} else {
		for _, dep := range bi.Deps {
			if dep.Path == modulePath {
				mod = dep
				break
			}
		}
	}
	if mod == nil || mod.Version == develModuleVersion || !semverPattern.MatchString(mod.Version) {
		return DevVersion
	}
	return strings.TrimPrefix(mod.Version, "v")
}

// vcsCommit returns the short VCS revision, marked dirty for modified trees.
func vcsCommit(settings map[string]string) string {
	rev := settings["vcs.revision"]
	if rev == "" {
		return Unknown
	}
	if len(rev) > shortRevisionLen {
		rev = rev[:shortRevisionLen]
	}
	if settings["vcs.modified"] == "true" {
		rev += dirtySuffix
	}
	return rev
}

func buildSettings(bi *debug.BuildInfo) map[string]string {
	settings := map[string]string{}
	if bi == nil {
		return settings
	}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	return settings
}
