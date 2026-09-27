package mitm

import (
	"bytes"
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

	"github.com/maci0/muninn-sidecar/internal/clock"
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
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
		"api.openai.com.":    "api.openai.com", // explicit DNS root
	}
	for in, want := range cases {
		got, err := normalizeHost(in)
		if err != nil {
			t.Errorf("normalizeHost(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Unicode (U-label) host cannot be named by a certificate SAN and never
// matches the punycode form a client actually connects with, so it is refused
// with an error naming the punycode spelling rather than deep inside x509.
func TestNormalizeHostRejectsNonASCII(t *testing.T) {
	if got, err := normalizeHost("münchen.de"); err == nil {
		t.Errorf("normalizeHost(non-ASCII) = %q, want an error", got)
	}
	if got, err := normalizeHost("xn--mnchen-3ya.de"); err != nil || got != "xn--mnchen-3ya.de" {
		t.Errorf("normalizeHost(punycode) = %q, %v; want the host unchanged", got, err)
	}
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LeafFor("münchen.de"); err == nil {
		t.Error("LeafFor should refuse a non-ASCII host")
	}
}

func TestLeafForRejectsOverlongHost(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 253 chars (DNS limit) is accepted; longer is rejected before minting.
	ok := strings.Repeat("a", 249) + ".com" // 253
	if _, err := ca.LeafFor(ok); err != nil {
		t.Errorf("253-char host should mint, got error: %v", err)
	}
	tooLong := strings.Repeat("a", maxHostLen+1)
	if _, err := ca.LeafFor(tooLong); err == nil {
		t.Errorf("host longer than %d should be rejected", maxHostLen)
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

func TestParseCAMismatchedKeyRegenerates(t *testing.T) {
	dir := t.TempDir()
	// Simulate an interrupted persistCA: a valid cert alongside a key from a
	// different generation. LoadOrCreateCA must regenerate, not reuse the pair.
	ca := mustGenCA(t)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := marshalKeyPEM(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ca-cert.pem"), ca.CertPEM(), 0o644)
	os.WriteFile(filepath.Join(dir, "ca-key.pem"), keyOut, 0o600)

	if _, err := parseCA(ca.CertPEM(), keyOut, Options{}); err == nil {
		t.Error("parseCA accepted a key that does not match the cert")
	}
	loaded, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("should regenerate on mismatched pair, got %v", err)
	}
	if string(loaded.CertPEM()) == string(ca.CertPEM()) {
		t.Error("mismatched CA pair was reused instead of regenerated")
	}
	if !loaded.key.PublicKey.Equal(loaded.cert.PublicKey) {
		t.Error("regenerated CA key does not match its cert")
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
	ca, err := generateCA(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// The CA's expiry is a calendar date, ten years out. A fixed 10*365*24h span
// falls short of that by the leap days in between, so a CA minted across a leap
// day would expire days before the ten years it advertises.
func TestCAValidityIsCalendarYears(t *testing.T) {
	ca := mustGenCA(t)

	notAfter := ca.cert.NotAfter
	// Undoing the ten calendar years must land back at the minting instant, which
	// is time.Now() modulo the test's own runtime: a fixed 10*365*24h span would
	// come back a few days short, by exactly the leap days in between.
	if back := notAfter.AddDate(-caValidityYears, 0, 0); back.Before(time.Now().Add(-time.Minute)) || back.After(time.Now().Add(time.Minute)) {
		t.Errorf("CA expires %v, which is not ten calendar years from now (%v)", notAfter, back)
	}
	if elapsed := notAfter.Sub(time.Now()); elapsed < time.Duration(caValidityYears)*365*24*time.Hour {
		t.Errorf("CA validity %v is shorter than %d nominal years", elapsed, caValidityYears)
	}
	if notAfter.Sub(ca.cert.NotBefore) <= 0 {
		t.Errorf("CA validity window is empty: %v to %v", ca.cert.NotBefore, notAfter)
	}
}

func FuzzNormalizeHost(f *testing.F) {
	f.Add("api.openai.com:443")
	f.Add("[::1]:443")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		got, err := normalizeHost(host)
		if err != nil {
			return // rejected input, nothing to check
		}
		if got != strings.ToLower(got) {
			t.Fatalf("result not lowercased: %q", got)
		}
		// Never panics; result has no surrounding whitespace.
		if got != strings.TrimSpace(got) {
			t.Fatalf("result not trimmed: %q", got)
		}
	})
}

// FuzzParseCA drives the on-disk CA load path. The pair is read from the
// user's config dir, so it is the one cryptographic parser in the proxy that
// takes bytes this process did not mint: a panic in pem.Decode, x509, or the
// key/cert match check kills the proxy at startup instead of on one request.
func FuzzParseCA(f *testing.F) {
	caPEM := func() (certPEM, keyPEM []byte) {
		ca, err := generateCA(Options{})
		if err != nil {
			f.Fatal(err)
		}
		keyPEM, merr := marshalKeyPEM(ca.key)
		if merr != nil {
			f.Fatal(merr)
		}
		return ca.CertPEM(), keyPEM
	}
	certPEM, keyPEM := caPEM()
	otherCert, otherKey := caPEM()
	if bytes.Equal(certPEM, otherCert) {
		f.Fatal("two generated CAs share a certificate")
	}

	// The matching pair, the mismatch pair parseCA must reject (an interrupted
	// persistCA leaves exactly this), a truncated copy of a valid pair, and the
	// non-PEM / wrong-block-type / empty shapes pem.Decode has to survive.
	f.Add(certPEM, keyPEM)
	f.Add(certPEM, otherKey)
	f.Add(certPEM[:len(certPEM)/2], keyPEM)
	f.Add([]byte("not pem at all"), []byte("nor this"))
	f.Add(keyPEM, certPEM) // block type EC PRIVATE KEY where CERTIFICATE is required
	f.Add(certPEM, []byte("-----BEGIN EC PRIVATE KEY-----\n-----END EC PRIVATE KEY-----\n"))
	f.Add([]byte(""), []byte(""))
	f.Fuzz(func(t *testing.T, certIn, keyIn []byte) {
		ca, err := parseCA(certIn, keyIn, Options{})
		if err != nil {
			if ca != nil {
				t.Fatalf("parseCA returned a CA alongside error %v", err)
			}
			return
		}
		if ca == nil || ca.cert == nil || ca.key == nil {
			t.Fatalf("parseCA succeeded with an incomplete CA: %+v", ca)
		}
		// The rejection the caller relies on to regenerate: a key that does not
		// belong to the cert would mint leaves whose signature never verifies.
		if !ca.key.PublicKey.Equal(ca.cert.PublicKey) {
			t.Fatal("parseCA accepted a key that does not match the cert")
		}
		if ca.cache == nil {
			t.Fatal("parseCA left the leaf cache nil; LeafFor would panic on insert")
		}
		// Persist/read boundary: the certPEM a CA hands back must be the one it
		// parsed, and re-encoding it must give back the same certificate.
		blk, _ := pem.Decode(ca.CertPEM())
		if blk == nil || blk.Type != "CERTIFICATE" {
			t.Fatalf("CertPEM is not a CERTIFICATE block: %q", ca.CertPEM())
		}
		again, perr := x509.ParseCertificate(blk.Bytes)
		if perr != nil {
			t.Fatalf("re-parsing CertPEM: %v", perr)
		}
		if !again.Equal(ca.cert) {
			t.Fatal("CertPEM does not round-trip to the parsed certificate")
		}
		// A CA that parsed must actually mint: this is the one thing a
		// never-panics assertion cannot cover.
		if _, err := ca.LeafFor("fuzz.example"); err != nil {
			t.Fatalf("LeafFor on a parsed CA: %v", err)
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

// scriptedOptions returns the clock a replaying run drives, parked at a fixed
// instant. Same script, same certificate dates.
func scriptedOptions() (*clock.Fake, Options) {
	f := clock.NewFake()
	return f, Options{Clock: f}
}

// Certificate dates must come from the injected clock, not the host's: a replay
// from a failing run's script has to reach the same validity windows, or the
// transcript that exposed an expiry bug is not reproducible.
//
// Key material and serials stay on crypto/rand and are deliberately not
// compared. Go's ECDSA key generation reseeds its DRBG from process-global
// state and draws a variable number of bytes, so no injected entropy source
// would make either reproducible.
func TestCertificateDatesComeFromInjectedClock(t *testing.T) {
	clkA, optsA := scriptedOptions()
	clkB, optsB := scriptedOptions()
	clkA.Advance(90 * 24 * time.Hour)
	clkB.Advance(90 * 24 * time.Hour)

	caA, err := LoadOrCreateCAWith(t.TempDir(), optsA)
	if err != nil {
		t.Fatal(err)
	}
	caB, err := LoadOrCreateCAWith(t.TempDir(), optsB)
	if err != nil {
		t.Fatal(err)
	}
	if !caA.cert.NotBefore.Equal(caB.cert.NotBefore) || !caA.cert.NotAfter.Equal(caB.cert.NotAfter) {
		t.Errorf("same scripted clock produced CA validity %v..%v and %v..%v",
			caA.cert.NotBefore, caA.cert.NotAfter, caB.cert.NotBefore, caB.cert.NotAfter)
	}
	if want := clkA.Now().Add(-certBackdate); !caA.cert.NotBefore.Equal(want) {
		t.Errorf("CA NotBefore = %v, want the scripted now less the backdate (%v)",
			caA.cert.NotBefore, want)
	}

	for _, host := range []string{"api.openai.com", "cli-chat-proxy.grok.com"} {
		leafA, err := caA.LeafFor(host)
		if err != nil {
			t.Fatal(err)
		}
		leafB, err := caB.LeafFor(host)
		if err != nil {
			t.Fatal(err)
		}
		if !leafA.Leaf.NotBefore.Equal(leafB.Leaf.NotBefore) || !leafA.Leaf.NotAfter.Equal(leafB.Leaf.NotAfter) {
			t.Errorf("same scripted clock produced different leaf validity for %q", host)
		}
		if want := clkA.Now().Add(leafValidity); !leafA.Leaf.NotAfter.Equal(want) {
			t.Errorf("leaf for %q NotAfter = %v, want the scripted now plus leafValidity (%v)",
				host, leafA.Leaf.NotAfter, want)
		}
	}
}

// Leaf lifetime is a clock decision, so a simulator has to be able to cross it
// without waiting a day: a session longer than leafValidity re-mints, and one
// that is not does not.
func TestLeafCacheFollowsInjectedClock(t *testing.T) {
	clk, opts := scriptedOptions()
	ca, err := LoadOrCreateCAWith(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ca.LeafFor("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}

	// Partway through the leaf's life the cached one is still served: same
	// pointer, so no re-mint happened.
	clk.Advance(leafValidity - time.Minute)
	again, err := ca.LeafFor("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("leaf re-minted before its validity elapsed")
	}

	// Past NotAfter the cached leaf is stale and a new one is minted.
	clk.Advance(2 * time.Minute)
	fresh, err := ca.LeafFor("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first {
		t.Error("expired leaf was served from the cache")
	}
	if !clk.Now().Before(fresh.Leaf.NotAfter) {
		t.Errorf("re-minted leaf expires %v, not after the scripted now %v",
			fresh.Leaf.NotAfter, clk.Now())
	}
}

// CA rotation is likewise a clock decision: a stored CA inside the renew window
// is replaced, one outside it is kept.
func TestStoredCARotationFollowsInjectedClock(t *testing.T) {
	dir := t.TempDir()
	clk, opts := scriptedOptions()
	ca, err := LoadOrCreateCAWith(dir, opts)
	if err != nil {
		t.Fatal(err)
	}

	// A reload on the same clock finds the stored CA still good and returns it
	// unchanged.
	early, err := LoadOrCreateCAWith(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(early.CertPEM()) != string(ca.CertPEM()) {
		t.Error("stored CA inside its validity was regenerated")
	}

	// Step the clock into the renew window and reload: rotation must happen
	// without the process being restarted. The jump is measured from the stored
	// CA's own NotAfter, not from a span of days: the CA's life is ten calendar
	// years, a different number of days depending on the leap days it spans.
	clk.Advance(ca.cert.NotAfter.Add(-caRenewBefore + time.Hour).Sub(clk.Now()))
	rotated, err := LoadOrCreateCAWith(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated.CertPEM()) == string(ca.CertPEM()) {
		t.Error("stale CA inside the renew window was reused")
	}
	if !rotated.cert.NotAfter.After(clk.Now()) {
		t.Errorf("rotated CA expires %v, not after the scripted now %v",
			rotated.cert.NotAfter, clk.Now())
	}
}
