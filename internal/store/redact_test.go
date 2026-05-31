package store

import (
	"strings"
	"testing"
)

// Test fixtures are assembled at runtime from fragments so no contiguous
// secret-shaped literal appears in source (which would trip secret scanners /
// push protection). They still match the redaction patterns once concatenated.

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name   string
		secret string // the credential-shaped token (built from parts)
		core   string // a distinctive substring that must NOT survive
	}{
		{"openai key", "sk-" + strings.Repeat("a", 30), strings.Repeat("a", 30)},
		{"openai proj key", "sk-proj-" + strings.Repeat("b", 30), strings.Repeat("b", 30)},
		{"anthropic key", "sk-ant-api03-" + strings.Repeat("c", 24), strings.Repeat("c", 24)},
		{"aws access key", "AKIA" + strings.Repeat("Z", 16), strings.Repeat("Z", 16)},
		{"github token", "gh" + "p_" + strings.Repeat("d", 36), strings.Repeat("d", 36)},
		{"google api key", "AI" + "za" + strings.Repeat("e", 35), strings.Repeat("e", 35)},
		{"slack token", "xo" + "xb-" + strings.Repeat("1", 12), strings.Repeat("1", 12)},
		{"bearer token", "Bearer " + strings.Repeat("f", 36), strings.Repeat("f", 36)},
		{"jwt", "ey" + "J" + strings.Repeat("a", 12) + ".ey" + "J" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12), strings.Repeat("b", 12)},
		{"private key block", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("M", 24) + "\n-----END RSA PRIVATE KEY-----", strings.Repeat("M", 24)},
		{"stripe secret key", "sk" + "_live_" + strings.Repeat("g", 24), strings.Repeat("g", 24)},
		{"stripe restricted key", "rk" + "_test_" + strings.Repeat("h", 24), strings.Repeat("h", 24)},
		{"github fine-grained pat", "github" + "_pat_" + strings.Repeat("i", 62), strings.Repeat("i", 62)},
		{"npm token", "npm" + "_" + strings.Repeat("j", 36), strings.Repeat("j", 36)},
		{"basic auth header", "Authorization: Basic " + strings.Repeat("k", 24), strings.Repeat("k", 24)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "prefix " + tc.secret + " suffix"
			got := redactSecrets(in)
			if !strings.Contains(got, redactionMarker) {
				t.Errorf("expected redaction marker, got %q", got)
			}
			if strings.Contains(got, tc.core) {
				t.Errorf("secret material survived redaction: %q", got)
			}
			if !strings.HasPrefix(got, "prefix ") || !strings.HasSuffix(got, " suffix") {
				t.Errorf("surrounding text damaged: %q", got)
			}
		})
	}

	// Legitimate prose must be left intact (no false positives).
	clean := []string{
		"Let's refactor the authentication module today.",
		"The function returns sk- prefixed ids? no.", // "sk-" without 20+ chars
		"bearer of bad news",                         // "bearer" without a token
		"pk" + "_live_" + strings.Repeat("x", 24),    // Stripe publishable key is public — keep
		"",
	}
	for _, c := range clean {
		if got := redactSecrets(c); got != c {
			t.Errorf("clean text altered: %q -> %q", c, got)
		}
	}
}

func TestRedactSecretsMultiple(t *testing.T) {
	k1 := "sk-" + strings.Repeat("a", 28)
	k2 := "AKIA" + strings.Repeat("Z", 16)
	in := "k1=" + k1 + " and k2=" + k2 + " end"
	got := redactSecrets(in)
	if strings.Contains(got, k1) || strings.Contains(got, k2) {
		t.Errorf("not all secrets redacted: %q", got)
	}
	if n := strings.Count(got, redactionMarker); n != 2 {
		t.Errorf("expected 2 redactions, got %d: %q", n, got)
	}
	if !strings.HasPrefix(got, "k1=") || !strings.HasSuffix(got, "end") {
		t.Errorf("surrounding text damaged: %q", got)
	}
}

func FuzzRedactSecrets(f *testing.F) {
	f.Add("sk-" + strings.Repeat("a", 25))
	f.Add("plain text with no secrets")
	f.Add("")
	f.Add("Bearer xyz")
	f.Fuzz(func(t *testing.T, s string) {
		got := redactSecrets(s)
		// Idempotence: the marker contains no secret pattern, so a second pass
		// must change nothing. (Also exercises no-panic on arbitrary input.)
		if got2 := redactSecrets(got); got2 != got {
			t.Fatalf("redaction not idempotent: %q -> %q -> %q", s, got, got2)
		}
		// A changed result always contains the marker; an unchanged result means
		// nothing matched.
		if got != s && !strings.Contains(got, redactionMarker) {
			t.Fatalf("changed input without inserting a marker: %q -> %q", s, got)
		}
	})
}
