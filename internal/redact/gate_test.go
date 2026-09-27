package redact

import (
	"strings"
	"testing"
)

// secretsUngated is the pre-gate reference: it applies every pattern and the
// email/kv rules unconditionally, with no necessary-condition checks. Secrets
// must agree with it on every input, which is what makes the gates in rule
// (literal, fold, numeric, keyStem) safe to have at all.
func secretsUngated(s string) string {
	if s == "" {
		return s
	}
	for _, p := range patterns {
		if p.re.MatchString(s) {
			s = p.re.ReplaceAllString(s, Marker)
		}
	}
	if emailPattern.MatchString(s) {
		s = emailPattern.ReplaceAllStringFunc(s, func(m string) string {
			if i := strings.IndexByte(m, ':'); i >= 0 {
				tail := m[i+1:]
				if strings.Contains(tail, "/") && !strings.Contains(tail, "@") {
					return m
				}
			}
			return Marker
		})
	}
	if kvPattern.MatchString(s) {
		s = kvPattern.ReplaceAllStringFunc(s, func(m string) string {
			sub := kvPattern.FindStringSubmatch(m)
			if strings.HasPrefix(strings.TrimLeft(sub[3], `"'`), Marker) {
				return m
			}
			return sub[1] + sub[2] + Marker
		})
	}
	return s
}

// TestSecretsMatchesUngatedReference pins the gates against the ungated
// reference on every fixture the other tests in this package build, plus
// inputs shaped to sit right at each gate's boundary.
func TestSecretsMatchesUngatedReference(t *testing.T) {
	cases := []string{
		"",
		"plain prose with no identifiers at all",
		"rotate every 2 hours, was 1 in 2024",
		"version 1.2.3.4 and 192.168.100.100 and 2026-09-27",
		"call (555) 123 4567 tomorrow",
		"sk-" + strings.Repeat("a", 30),
		"xai-" + strings.Repeat("z", 24),
		"BEARER " + strings.Repeat("f", 36),
		"Authorization: BASIC " + strings.Repeat("k", 24),
		"sk" + "_LIVE_" + strings.Repeat("g", 24),
		"4111" + "1111" + "1111" + "1111",
		"4111 1111 1111 1111",
		"123-45-6789",
		"+" + "1 (555) 123 4567",
		"alice.dev" + "@" + "example.com",
		"git" + "@" + "github.com:org/repo.git",
		"DB" + "_PASSWORD" + "=s3cr3tvalue",
		"aws_secret_access_key = " + strings.Repeat("x", 40),
		"the secret to success and the key to the kingdom",
		"no secret here, but a token: " + strings.Repeat("q", 9),
		"credentials: hunter2passphrase",
		"sk-abc " + strings.Repeat("d", 36), // xai- and sk- gates in one string
	}
	for _, c := range cases {
		if got, want := Secrets(c), secretsUngated(c); got != want {
			t.Errorf("Secrets(%q) = %q, ungated reference = %q", c, got, want)
		}
	}
}

// FuzzSecretsMatchesUngatedReference is the standing check that no gate can
// ever reject a string one of the patterns would have matched.
func FuzzSecretsMatchesUngatedReference(f *testing.F) {
	f.Add("sk-" + strings.Repeat("a", 30))
	f.Add("4111 1111 1111 1111")
	f.Add("PASSWORD" + "=letmein99")
	f.Add("alice" + "@" + "example.com")
	f.Add("no secrets here")
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := Secrets(s), secretsUngated(s); got != want {
			t.Fatalf("Secrets(%q) = %q, ungated reference = %q", s, got, want)
		}
	})
}

// TestNumericCandidateHoldsForEveryNumericMatch checks the shared numeric gate
// directly: each pattern that carries it must match only when the gate holds.
func TestNumericCandidateHoldsForEveryNumericMatch(t *testing.T) {
	matches := []string{
		"4111" + "1111" + "1111" + "1111",
		"5105 1051 0510 5100",
		"2221" + strings.Repeat("0", 13),
		"123-45-6789",
		"+" + "44 20 7946 0958",
		"+" + "1 (555) 123 4567",
		"+" + "15551234567",
		"555-123-4567",
		"(555) 123 4567",
	}
	for _, m := range matches {
		if !numericCandidate(m) {
			t.Errorf("numericCandidate(%q) = false, want true", m)
		}
	}
}
