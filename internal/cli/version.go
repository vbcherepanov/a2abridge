package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/vbcherepanov/a2abridge/internal/buildinfo"
)

// RunVersion prints semver, commit, build date, Go runtime and target os/arch.
func RunVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout,
		"a2abridge %s\ncommit:    %s\nbuilt:     %s\ngo:        %s\nplatform:  %s/%s\n",
		buildinfo.Version,
		buildinfo.Commit,
		buildinfo.BuildDate,
		runtime.Version(),
		runtime.GOOS, runtime.GOARCH,
	)
	return 0
}
