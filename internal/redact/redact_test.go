package redact

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
		{"xai key", "xai" + "-" + strings.Repeat("z", 24), strings.Repeat("z", 24)},
		{"google oauth token", "ya29" + "." + strings.Repeat("Q", 30), strings.Repeat("Q", 30)},
		{"bearer token", "Bearer " + strings.Repeat("f", 36), strings.Repeat("f", 36)},
		{"jwt", "ey" + "J" + strings.Repeat("a", 12) + ".ey" + "J" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12), strings.Repeat("b", 12)},
		{"private key block", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("M", 24) + "\n-----END RSA PRIVATE KEY-----", strings.Repeat("M", 24)},
		{"stripe secret key", "sk" + "_live_" + strings.Repeat("g", 24), strings.Repeat("g", 24)},
		{"stripe restricted key", "rk" + "_test_" + strings.Repeat("h", 24), strings.Repeat("h", 24)},
		{"github fine-grained pat", "github" + "_pat_" + strings.Repeat("i", 62), strings.Repeat("i", 62)},
		{"npm token", "npm" + "_" + strings.Repeat("j", 36), strings.Repeat("j", 36)},
		{"basic auth header", "Authorization: Basic " + strings.Repeat("k", 24), strings.Repeat("k", 24)},
		{"email address", "alice.dev" + "@" + "example.com", "alice.dev" + "@" + "example.com"},
		{"email plus tag", "u.ser+tag" + "@" + "sub.corp.co.uk", "u.ser+tag" + "@" + "sub.corp.co.uk"},
		{"email password combo", "john.doe" + "@" + "example.com:hunter2pass99", "example.com:hunter2pass99"},
		{"email then colon word", "alice" + "@" + "example.com:signed", "alice" + "@" + "example.com"},
		{"url embedded credentials", "https://user" + "@" + "example.com:pass" + "@" + "host.com/x", "example.com:pass"},
		{"visa card", "4111" + "1111" + "1111" + "1111", "4111111111111111"},
		{"mastercard card", "5500" + "0000" + "0000" + "0004", "5500000000000004"},
		{"amex card", "3782" + "822463" + "10005", "378282246310005"},
		{"visa card spaced", "4111 1111 1111 1111", "4111 1111 1111 1111"},
		{"visa card dashed", "4111-1111-1111-1111", "4111-1111-1111-1111"},
		{"ssn dashed", "123-45-6789", "123-45-6789"},
		{"phone nanp dashed", "415-555-0132", "415-555-0132"},
		{"phone nanp spaced", "212 555 0184", "212 555 0184"},
		{"phone nanp parenthesized", "(212) 555-0184", "212) 555-0184"},
		{"phone nanp with country code", "+1 415 555 0132", "415 555 0132"},
		{"phone nanp with country code parenthesized", "+1 (415) 555-0132", "415) 555-0132"},
		{"phone toll free dashed", "1-800-555-0199", "800-555-0199"},
		{"phone e164", "+442079460958", "442079460958"},
		{"phone e164 grouped", "+44 20 7946 0958", "7946 0958"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "prefix " + tc.secret + " suffix"
			got := Secrets(in)
			if !strings.Contains(got, Marker) {
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
		"The function returns sk- prefixed ids? no.",           // "sk-" without 20+ chars
		"bearer of bad news",                                   // "bearer" without a token
		"pk" + "_live_" + strings.Repeat("x", 24),              // Stripe publishable key is public, keep
		"email me at the office",                               // "email" word without an address
		"version 1.2.3 of the package",                         // dotted numbers are not an email
		"order id 1234567890123456 shipped",                    // 16 digits but not a card prefix (starts with 1)
		"build 8888-0000-0000-0000 tagged",                     // dashed groups but not a card prefix
		"ticket 12-34-5678 resolved",                           // not the SSN 3-2-4 grouping
		"version 1.2.3.4 of the toolchain",                     // dotted version, not a phone
		"host 192.168.100.100 is the staging box",              // IPv4, dots never group a phone
		"released 2026-09-27, run 2026-09-27T10:00:00Z",        // ISO date and timestamp
		"order 1234 5678 9012 shipped",                         // 4-4-4 grouping is not NPA-NXX-XXXX
		"invoice 2026 400 5000 net 30",                         // exchange code may not start with 0
		"account 1234567890",                                   // an ungrouped 10-digit run is left alone
		"deploy task-management-service-prod now",              // "sk-" inside a word is not a key
		"galaxai-" + strings.Repeat("m", 24),                   // "xai-" inside a word is not a key
		"git clone git" + "@" + "github.com:org/repo.git",      // scp-style remote, not an email
		"rsync app deploy" + "@" + "prod.example.com:/var/www", // scp target, not an email
		"",
	}
	for _, c := range clean {
		if got := Secrets(c); got != c {
			t.Errorf("clean text altered: %q -> %q", c, got)
		}
	}
}

func TestRedactKeyValueSecrets(t *testing.T) {
	// The dominant leak vector: pasted .env files / exports. Value is redacted,
	// key kept for context.
	redacted := []struct{ name, in string }{
		{"env api key", "API_KEY=supersecretvalue123"},
		{"export with quotes", `export DB_PASSWORD="hunter2-longer-pw"`},
		{"yaml colon", "password: my-secret-passphrase"},
		{"json client secret", `"client_secret": "abc123def456ghi"`},
		{"access token", "access_token = abcdef1234567890"},
		{"private key var", "PRIVATE_KEY=longprivatekeymaterialxyz"},
		{"quoted multi-word value", `DB_PASSWORD="correct horse battery"`},
		{"quoted short first word", `PASSWORD="my secret pass"`},
		{"single-quoted multi-word", `export SECRET='p@ss word1 word2'`},
		{"unterminated quote", `password="abcdefgh12345`},
		{"unterminated quote export", `export DB_PASSWORD="supersecretvalue123`},
	}
	for _, tc := range redacted {
		t.Run(tc.name, func(t *testing.T) {
			got := Secrets(tc.in)
			if !strings.Contains(got, Marker) {
				t.Errorf("expected redaction, got %q", got)
			}
			// The key name must survive (context); the value must not.
			for _, secretVal := range []string{"supersecretvalue123", "hunter2-longer-pw", "my-secret-passphrase", "abc123def456ghi", "abcdef1234567890", "longprivatekeymaterialxyz", "correct horse battery", "my secret pass", "p@ss word1 word2", "abcdefgh12345"} {
				if strings.Contains(got, secretVal) {
					t.Errorf("value leaked: %q", got)
				}
			}
		})
	}

	// No false positives: prose, trivial/short values, non-sensitive keys.
	clean := []string{
		"the secret to success is hard work", // no := after "secret"
		"token=1",                            // value too short
		"password=",                          // no value
		"width=1200px",                       // non-sensitive key
		"tokens=5 returned",                  // "tokens" != "token" (word boundary)
		`password="abc"`,                     // quoted value under the 6-char minimum
		"discuss the api_key design",         // no separator+value
	}
	for _, c := range clean {
		if got := Secrets(c); got != c {
			t.Errorf("false positive: %q -> %q", c, got)
		}
	}
}

func TestRedactSecretsMultiple(t *testing.T) {
	k1 := "sk-" + strings.Repeat("a", 28)
	k2 := "AKIA" + strings.Repeat("Z", 16)
	in := "k1=" + k1 + " and k2=" + k2 + " end"
	got := Secrets(in)
	if strings.Contains(got, k1) || strings.Contains(got, k2) {
		t.Errorf("not all secrets redacted: %q", got)
	}
	if n := strings.Count(got, Marker); n != 2 {
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
	f.Add("contact alice" + "@" + "example.com please")
	f.Add("card 4111" + "1111" + "1111" + "1111 on file")
	f.Add("ssn 123-45-6789 on record")
	f.Add("login bob" + "@" + "example.com:hunter2pass99 now")
	f.Add(`password="abcdefgh12345`)
	f.Add(`pAsswd="000000"0`) // quoted match leaves a stray byte; second pass must not re-redact
	f.Fuzz(func(t *testing.T, s string) {
		got := Secrets(s)
		// Idempotence: the marker contains no secret pattern, so a second pass
		// must change nothing. (Also exercises no-panic on arbitrary input.)
		if got2 := Secrets(got); got2 != got {
			t.Fatalf("redaction not idempotent: %q -> %q -> %q", s, got, got2)
		}
		// A changed result always contains the marker; an unchanged result means
		// nothing matched.
		if got != s && !strings.Contains(got, Marker) {
			t.Fatalf("changed input without inserting a marker: %q -> %q", s, got)
		}
	})
}

// TestRedactInternationalEmail covers addresses an ASCII-only grammar misses
// entirely: a non-ASCII local part, an IDN domain in display form, and a
// decomposed (NFD) spelling as macOS filesystems and IMAP servers hand out.
// Each of these reached long-term memory unredacted before the pattern took
// Unicode classes.
func TestRedactInternationalEmail(t *testing.T) {
	// NFD: "jose" + combining acute, the form a macOS HFS+/APFS filename or an
	// Apple Mail address arrives in. Assembled at runtime so the source file
	// stays in one normalization form.
	nfdJose := "jos" + "e" + string(rune(0x0301))
	cases := []struct {
		name  string
		email string
	}{
		{"cyrillic domain", "user" + "@" + "почта.рф"},
		{"accented local part", "jos" + string(rune(0x00E9)) + "@" + "dömain.de"},
		{"decomposed local part", nfdJose + "@" + "example.com"},
		{"all non-ascii", "почта" + "@" + "почта.рф"},
		{"non-ascii with password tail", "user" + "@" + "почта.рф:hunter2pass99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "reach me at " + tc.email + " any time"
			got := Secrets(in)
			if strings.Contains(got, tc.email) {
				t.Errorf("address survived redaction: %q", got)
			}
			if !strings.Contains(got, Marker) {
				t.Errorf("no marker inserted: %q", got)
			}
			if !strings.HasPrefix(got, "reach me at ") || !strings.HasSuffix(got, " any time") {
				t.Errorf("surrounding text damaged: %q", got)
			}
		})
	}

	// A punycode TLD and an scp remote both still classify correctly.
	if got := Secrets("clone git" + "@" + "github.com:org/repo.git"); strings.Contains(got, Marker) {
		t.Errorf("scp remote redacted: %q", got)
	}
}
