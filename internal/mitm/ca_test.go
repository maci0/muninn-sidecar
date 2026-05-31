package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestCARegeneratesNearExpiry(t *testing.T) {
	dir := t.TempDir()
	// Seed a CA cert/key that expires within the renew window.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := randomSerial()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "muninn-sidecar local CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour), // within caRenewBefore
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyOut, _ := marshalKeyPEM(key)
	os.WriteFile(filepath.Join(dir, "ca-cert.pem"), certPEM, 0o644)
	os.WriteFile(filepath.Join(dir, "ca-key.pem"), keyOut, 0o600)

	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Must have regenerated a long-lived CA, not reused the near-expired one.
	if !ca.cert.NotAfter.After(time.Now().Add(caRenewBefore)) {
		t.Errorf("expected a freshly regenerated CA, got NotAfter=%v", ca.cert.NotAfter)
	}
	if string(ca.CertPEM()) == string(certPEM) {
		t.Error("near-expiry CA was reused instead of regenerated")
	}
}

func TestLeafReMintOnExpiry(t *testing.T) {
	ca := mustGenCA(t)
	// Inject an already-expired cached leaf.
	ca.cache["example.com"] = &tls.Certificate{Leaf: &x509.Certificate{NotAfter: time.Now().Add(-time.Hour)}}

	leaf, err := ca.LeafFor("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf == nil || !leaf.Leaf.NotAfter.After(time.Now()) {
		t.Error("expired cached leaf was not re-minted")
	}
	if len(leaf.Certificate) != 2 {
		t.Errorf("re-minted leaf missing CA in chain: %d certs", len(leaf.Certificate))
	}
}

func TestLeafCacheBounded(t *testing.T) {
	ca := mustGenCA(t)
	// Fill the cache to capacity with dummy valid entries.
	future := time.Now().Add(time.Hour)
	for i := 0; i < maxCacheEntries; i++ {
		ca.cache[fmt.Sprintf("host-%d.example", i)] = &tls.Certificate{Leaf: &x509.Certificate{NotAfter: future}}
	}
	// Minting a new host must evict, keeping the cache bounded.
	if _, err := ca.LeafFor("brand-new.example"); err != nil {
		t.Fatal(err)
	}
	if len(ca.cache) > maxCacheEntries {
		t.Errorf("cache grew past cap: %d > %d", len(ca.cache), maxCacheEntries)
	}
}

func TestLeafForConcurrent(t *testing.T) {
	ca := mustGenCA(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Mix of repeated and distinct hosts to exercise cache hits + inserts.
			host := fmt.Sprintf("host-%d.example", n%10)
			leaf, err := ca.LeafFor(host)
			if err != nil || leaf == nil || leaf.Leaf == nil {
				t.Errorf("LeafFor(%q): leaf=%v err=%v", host, leaf, err)
			}
		}(i)
	}
	wg.Wait()
}

func mustGenCA(t *testing.T) *CA {
	t.Helper()
	ca, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
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
