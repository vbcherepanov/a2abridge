package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/vbcherepanov/a2abridge/internal/security"
)

func init() {
	registerCommand(Command{
		Name:    "cert",
		Summary: "Generate ed25519 cert+key for cross-machine federation (Phase 2)",
		Run:     RunCert,
	})
}

// RunCert handles `a2abridge cert generate [flags]`. We intentionally
// keep this as a sub-action even though there's only one — a future
// `cert verify` / `cert rotate` will slot in cleanly.
func RunCert(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printCertUsage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "generate":
		return certGenerate(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "a2abridge cert: unknown action %q\n\n", args[0])
		printCertUsage(stderr)
		return 2
	}
}

func printCertUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: a2abridge cert <action> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  generate   Create an ed25519 self-signed cert + key for federation")
}

func certGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cert generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := fs.String("cn", "", "common name (defaults to hostname)")
	dirFlag := fs.String("dir", "", "output directory (default: ~/.a2abridge/tls)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cnVal := *cn
	if cnVal == "" {
		host, err := os.Hostname()
		if err == nil {
			cnVal = host
		} else {
			cnVal = "a2abridge-peer"
		}
	}

	out := *dirFlag
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "cert generate: home dir: %v\n", err)
			return 1
		}
		out = filepath.Join(home, ".a2abridge", "tls")
	}

	certPath, keyPath, err := security.GenerateEd25519Cert(out, cnVal)
	if err != nil {
		fmt.Fprintf(stderr, "cert generate: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout,
		"generated ed25519 cert + key for %q\n  cert: %s\n  key:  %s\n\n"+
			"Wire it into your bridge by setting in ~/.claude/settings.json mcpServers.a2a.env:\n"+
			"  A2A_TLS_CERT=%s\n  A2A_TLS_KEY=%s\n  A2A_TRUST_ROOTS=%s   # add peer certs as a colon-separated list\n",
		cnVal, certPath, keyPath, certPath, keyPath, certPath,
	)
	return 0
}
