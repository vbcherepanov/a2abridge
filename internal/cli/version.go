package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/vbcherepanov/a2abridge/v4/internal/buildinfo"
)

// RunVersion prints semver, commit, build date, Go runtime and target os/arch.
func RunVersion(args []string, stdout, _ io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, "Usage: a2abridge version")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Print version, commit, build date, Go runtime and target platform.")
		return 0
	}
	info := buildinfo.Get()
	fmt.Fprintf(stdout,
		"a2abridge %s\ncommit:    %s\nbuilt:     %s\ngo:        %s\nplatform:  %s/%s\n",
		info.Version,
		info.Commit,
		info.BuildDate,
		runtime.Version(),
		runtime.GOOS, runtime.GOARCH,
	)
	return 0
}
