package redact

import (
	"net/url"
	"testing"
)

// FuzzSecrets drives the secret scanner over untrusted text. Everything that
// reaches Secrets arrives from a captured agent turn, so the input is attacker
// shaped as much as it is a pasted credential: tool output, model text, a
// truncated .env, a megabyte of a stack trace.
//
// Invariants: a non-empty input never redacts down to nothing (an empty
// capture stores no context at all, so wiping the string is a data-destroying
// bug, not a safe failure), and redacting an already-redacted string is a
// no-op, so a body captured twice redacts to the same bytes both times.
func FuzzSecrets(f *testing.F) {
	f.Add("AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	f.Add("password=\"correct horse battery staple\"")
	f.Add("contact me at alice@example.com or bob@exämple.de")
	f.Add("ssh git@github.com:muninn/muninn.git")
	f.Add("cd /home/alice/src && cat .env")
	f.Add("token=abc123def456")
	f.Add("no secrets here, just prose with an @ in it")
	f.Add("")
	f.Add("api_key=[REDACTED]")
	f.Fuzz(func(t *testing.T, s string) {
		got := Secrets(s)
		if s != "" && got == "" {
			t.Fatalf("redaction emptied a non-empty input (%d bytes): %q", len(s), s)
		}
		if again := Secrets(got); again != got {
			t.Fatalf("redaction not idempotent:\n in %q\n 1st %q\n 2nd %q", s, got, again)
		}
	})
}

// FuzzURL checks the credential-carrying URL form. A userinfo password is
// what URL exists to remove, so an input that parses and carries one must not
// survive in the output; the host, scheme, and path are what make a log line
// diagnosable and must stay.
func FuzzURL(f *testing.F) {
	f.Add("https://user:hunter2@mcp.example.com/mcp")
	f.Add("https://token@example.com/v1?key=abc")
	f.Add("http://user@example.com:8080/path")
	f.Add("not a url at all")
	f.Add("")
	f.Add("https://user:p@ss w0rd@host/path")
	f.Fuzz(func(t *testing.T, raw string) {
		got := URL(raw)
		u, err := url.Parse(raw)
		if err != nil {
			// An unparseable URL is returned verbatim by design.
			return
		}
		if u.User == nil {
			return
		}
		pass, hasPass := u.User.Password()
		if !hasPass || pass == "" {
			return
		}
		if got == "" {
			t.Fatalf("URL emptied a non-empty input: %q", raw)
		}
		// A substring test would be meaningless here: a one-character password
		// occurs inside almost any host. Compare the password the output
		// actually parses with, which is the value that reaches a log line.
		out, err := url.Parse(got)
		if err != nil {
			return
		}
		if out.User == nil {
			return
		}
		if leaked, ok := out.User.Password(); ok && leaked == pass {
			t.Fatalf("URL leaked userinfo password %q: %q -> %q", pass, raw, got)
		}
		if u.Host != "" && out.Host == "" {
			t.Fatalf("URL dropped the host: %q -> %q", raw, got)
		}
	})
}
