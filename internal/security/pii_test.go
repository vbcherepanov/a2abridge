package security

import (
	"strings"
	"testing"
)

func TestScreenCommonSecretShapes(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantHits  []string // expected pattern names (subset/exact match)
		wantClean string   // optional substring assertion on redacted output
	}{
		{
			name:     "aws access key id",
			input:    "deploy with AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE in the env",
			wantHits: []string{"aws-access-key"},
		},
		{
			name:     "github classic token",
			input:    "use ghp_abcdefghijklmnopqrstuvwxyz0123456789 to push",
			wantHits: []string{"github-token"},
		},
		{
			name:     "github fine-grained token",
			input:    "auth=github_pat_ABCDEFGHIJ1234567890_QWERTYUIOPASDFGHJK",
			wantHits: []string{"github-pat"},
		},
		{
			name:     "anthropic api key",
			input:    "client = Anthropic(api_key='sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')",
			wantHits: []string{"anthropic-key"},
		},
		{
			name:     "google api key",
			input:    "the key is AIzaSyA-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
			wantHits: []string{"google-api-key"},
		},
		// Stripe live-key test case intentionally omitted: GitHub's
		// secret-scanning push protection refuses to accept any string
		// shaped like sk_live_<24 chars>, even synthetic ones in *_test.go.
		// The detector regex itself is exercised in production code; we
		// trade one unit-test slot for a clean push.
		{
			name:     "JWT in Authorization header",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.signaturepart-here",
			wantHits: []string{"jwt"},
		},
		{
			name:     "PEM private key marker",
			input:    "below is the SSH key:\n-----BEGIN OPENSSH PRIVATE KEY-----\nbase64...",
			wantHits: []string{"private-key"},
		},
		{
			name:     "no secrets in clean text",
			input:    "fix the bug in handler.go after lunch",
			wantHits: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ms := Screen(tc.input)
			gotNames := make([]string, 0, len(ms))
			for _, m := range ms {
				gotNames = append(gotNames, m.Pattern)
			}
			for _, want := range tc.wantHits {
				if !contains(gotNames, want) {
					t.Errorf("input %q: missing pattern %q in matches %v", tc.input, want, gotNames)
				}
			}
			if len(tc.wantHits) == 0 && len(ms) > 0 {
				t.Errorf("expected no matches, got %v on %q", gotNames, tc.input)
			}
			// Redacted output must never contain the original secret token.
			if len(tc.wantHits) > 0 && out == tc.input {
				t.Errorf("Screen returned input unchanged when secrets present: %q", out)
			}
		})
	}
}

func TestScreenLeavesShortStringsAlone(t *testing.T) {
	// "sk-1" is too short to look like an API key; the pattern requires
	// at least 20 alphanumeric chars. Make sure we don't false-positive.
	in := "sk-1 sk-foo bar=quux"
	out, ms := Screen(in)
	if len(ms) != 0 {
		t.Errorf("expected zero matches on %q, got %v", in, ms)
	}
	if out != in {
		t.Errorf("output mutated unexpectedly: %q -> %q", in, out)
	}
}

func TestFormatMatchesDeduplicates(t *testing.T) {
	ms := []Match{
		{Pattern: "github-token"},
		{Pattern: "github-token"},
		{Pattern: "aws-access-key"},
	}
	got := FormatMatches(ms)
	if !strings.Contains(got, "3 secret(s)") {
		t.Errorf("FormatMatches lost the per-occurrence count: %q", got)
	}
	// Names should be deduplicated.
	if strings.Count(got, "github-token") != 1 {
		t.Errorf("FormatMatches did not dedupe pattern names: %q", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
