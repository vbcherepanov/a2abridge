// Package buildinfo exposes build-time identification.
package buildinfo

// Version is the released semver. Overridable at link time:
//
//	go build -ldflags "-X github.com/vbcherepanov/a2abridge/internal/buildinfo.Version=1.2.3"
var Version = "0.2.0-dev"

// Commit is set at link time to the short git SHA of the build.
var Commit = "unknown"

// BuildDate is set at link time to ISO-8601 build date.
var BuildDate = "unknown"
