// Package security implements PII / secret scrubbing applied to messages
// before they leave this agent. The screen is intentionally
// over-aggressive — false positives are tolerable, false negatives are
// not (a leaked AWS key is far worse than a refused message).
package security

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a labeled regex used by the PII screener. Adding a new
// detector is a matter of appending to defaultPatterns().
type Pattern struct {
	Name  string
	Re    *regexp.Regexp
}

// Match describes one redaction performed on the input.
type Match struct {
	Pattern string
	Value   string // truncated value for diagnostics; never the full secret
}

// Screen scans s and returns:
//   - the redacted string with each match replaced by `[REDACTED:<name>]`;
//   - a slice of Match describing what was hit (so callers can decide
//     whether to log a warning or refuse to send).
//
// Empty input → empty output, no matches.
func Screen(s string) (string, []Match) {
	if s == "" {
		return s, nil
	}
	patterns := defaultPatterns()
	var matches []Match
	out := s
	for _, p := range patterns {
		out = p.Re.ReplaceAllStringFunc(out, func(hit string) string {
			matches = append(matches, Match{
				Pattern: p.Name,
				Value:   truncate(hit, 12),
			})
			return fmt.Sprintf("[REDACTED:%s]", p.Name)
		})
	}
	return out, matches
}

// HasSecrets is a fast no-allocate predicate — useful when the caller
// only needs a yes/no answer (e.g. a doctor check).
func HasSecrets(s string) bool {
	for _, p := range defaultPatterns() {
		if p.Re.MatchString(s) {
			return true
		}
	}
	return false
}

// defaultPatterns is the active screener set. Patterns are intentionally
// scoped to high-value secrets that are uniquely shaped — generic strings
// like "password" or "secret" are NOT screened because the false-positive
// rate would dominate. Add new patterns conservatively and document why.
func defaultPatterns() []Pattern {
	mp := func(name, expr string) Pattern {
		return Pattern{Name: name, Re: regexp.MustCompile(expr)}
	}
	return []Pattern{
		// AWS access key id — fixed prefix + 16 base32 chars.
		mp("aws-access-key", `AKIA[0-9A-Z]{16}`),
		// AWS secret access key — only screens lines explicitly marked
		// as secrets to avoid catching every 40-char base64 in code.
		mp("aws-secret", `(?i)aws[_-]?secret[_-]?access[_-]?key["' :=]+[A-Za-z0-9/+=]{40}`),
		// GitHub personal/access tokens — both classic and fine-grained.
		mp("github-token", `gh[pousr]_[A-Za-z0-9_]{36,}`),
		// GitHub fine-grained tokens.
		mp("github-pat", `github_pat_[A-Za-z0-9_]{22,}`),
		// Anthropic comes BEFORE openai because openai's "sk-..." prefix
		// is a strict prefix of "sk-ant-..."; ordering ensures we report
		// the more specific name when the input is an Anthropic key.
		mp("anthropic-key", `sk-ant-[A-Za-z0-9_-]{20,}`),
		mp("openai-key", `sk-[A-Za-z0-9_-]{20,}`),
		// Google API keys.
		mp("google-api-key", `AIza[0-9A-Za-z_-]{35}`),
		// Slack tokens.
		mp("slack-token", `xox[baprs]-[A-Za-z0-9-]{10,}`),
		// Stripe live secret keys.
		mp("stripe-key", `sk_live_[A-Za-z0-9]{24,}`),
		// JWT — three base64url segments separated by dots.
		mp("jwt", `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		// PEM-formatted private keys (any flavour: RSA, EC, OPENSSH, PGP).
		mp("private-key", `-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED |PGP )?PRIVATE KEY-----`),
	}
}

// truncate returns at most n chars of s with an ellipsis on overflow.
// Used by Screen so diagnostic output never echoes the whole secret.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "..."
}

// FormatMatches renders a list of matches in a single human line, useful
// for log messages: "redacted 2 secret(s): aws-access-key, github-token".
func FormatMatches(ms []Match) string {
	if len(ms) == 0 {
		return ""
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		if !seen[m.Pattern] {
			seen[m.Pattern] = true
			names = append(names, m.Pattern)
		}
	}
	return fmt.Sprintf("redacted %d secret(s): %s", len(ms), strings.Join(names, ", "))
}
