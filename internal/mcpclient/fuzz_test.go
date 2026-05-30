package mcpclient

import "testing"

// FuzzHealthURLFrom exercises the URL-derivation surface with arbitrary input.
// Invariant: never panic; on success the derived health URL must re-parse.
func FuzzHealthURLFrom(f *testing.F) {
	f.Add("http://127.0.0.1:8750/mcp")
	f.Add("https://example.com/mcp/")
	f.Add("")
	f.Add("://bad")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := healthURLFrom(raw)
		if err != nil {
			return
		}
		if _, err := healthURLFrom(got); err != nil {
			t.Fatalf("derived health URL %q does not re-parse: %v", got, err)
		}
	})
}
