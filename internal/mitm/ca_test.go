package mitm

import (
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateCAPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Files written with the right perms.
	if fi, err := os.Stat(filepath.Join(dir, "ca-key.pem")); err != nil {
		t.Fatalf("ca key not written: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("ca key perms = %v, want 0600", fi.Mode().Perm())
	}
	// Reload returns the SAME CA (cert bytes identical), not a regenerated one.
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Error("reloaded CA differs from persisted one")
	}
}

func TestLeafChainsToCA(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.LeafFor("api.openai.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf == nil {
		t.Fatal("leaf.Leaf not populated")
	}
	// The leaf must verify against the CA for the host, with no system roots.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not add CA to pool")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.openai.com", Roots: roots}); err != nil {
		t.Fatalf("leaf does not chain to CA: %v", err)
	}
	// The chain must include the CA cert so a client can build the path.
	if len(leaf.Certificate) != 2 {
		t.Errorf("expected leaf+CA in the chain, got %d certs", len(leaf.Certificate))
	}
}

func TestLeafCachedPerHost(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := ca.LeafFor("example.com")
	b, _ := ca.LeafFor("EXAMPLE.com:443") // same host after normalization
	if a != b {
		t.Error("expected the same cached leaf for the normalized host")
	}
	c, _ := ca.LeafFor("other.com")
	if a == c {
		t.Error("different hosts must get different leaves")
	}
}

func TestLeafIPSAN(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())
	leaf, err := ca.LeafFor("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Leaf.IPAddresses) != 1 || !leaf.Leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("expected an IP SAN, got DNS=%v IP=%v", leaf.Leaf.DNSNames, leaf.Leaf.IPAddresses)
	}
	if len(leaf.Leaf.DNSNames) != 0 {
		t.Errorf("IP host should not get a DNS SAN: %v", leaf.Leaf.DNSNames)
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"API.OpenAI.com:443": "api.openai.com",
		"api.openai.com":     "api.openai.com",
		"  Host:8080 ":       "host",
		"[::1]:443":          "::1",
		"[2001:db8::1]":      "2001:db8::1",
		"127.0.0.1:443":      "127.0.0.1",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCACorruptRegenerates(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed corrupt files; LoadOrCreateCA must regenerate rather than fail.
	os.WriteFile(filepath.Join(dir, "ca-cert.pem"), []byte("garbage"), 0o644)
	os.WriteFile(filepath.Join(dir, "ca-key.pem"), []byte("garbage"), 0o600)
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("should regenerate on corrupt CA, got %v", err)
	}
	if len(ca.CertPEM()) == 0 {
		t.Error("regenerated CA has empty cert")
	}
}

func FuzzNormalizeHost(f *testing.F) {
	f.Add("api.openai.com:443")
	f.Add("[::1]:443")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		got := normalizeHost(host)
		if got != strings.ToLower(got) {
			t.Fatalf("result not lowercased: %q", got)
		}
		// Never panics; result has no surrounding whitespace.
		if got != strings.TrimSpace(got) {
			t.Fatalf("result not trimmed: %q", got)
		}
	})
}

func FuzzLeafFor(f *testing.F) {
	ca, err := LoadOrCreateCA(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Add("api.openai.com")
	f.Add("127.0.0.1:443")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		// Must never panic for any host string; empty host is the only expected
		// error path. A returned leaf must always carry a parsed Leaf cert.
		leaf, err := ca.LeafFor(host)
		if err != nil {
			return
		}
		if leaf.Leaf == nil {
			t.Fatalf("leaf for %q has nil Leaf", host)
		}
	})
}
