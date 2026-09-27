// Package mitm provides the certificate authority and per-host leaf-certificate
// minting for msc's opt-in TLS interception mode. When `--mitm` is enabled, msc
// acts as an HTTPS CONNECT proxy: it terminates TLS using a leaf certificate
// minted on the fly (signed by a locally-generated CA the agent is told to
// trust), runs the normal recall/inject + capture pipeline on the decrypted
// request, then re-originates TLS to the real upstream. This lets msc intercept
// agents that don't honor a base-URL env override (e.g. codex in
// ChatGPT-subscription mode, grok session auth) and is the groundwork for using
// msc as a transparent HTTPS_PROXY.
//
// Security: the CA private key is generated locally, stored 0600 under the user's
// config dir, and never leaves the machine. Trust is scoped — only the child
// agent process is told to trust it (via NODE_EXTRA_CA_CERTS / SSL_CERT_FILE),
// not the system trust store. MITM is off by default and strictly opt-in.
//
// Every source of *time* this package's output depends on is injected: validity
// windows, leaf-cache expiry, and CA rotation all read a clock.Clock.
// Production passes the zero Options and gets the host clock; a test or
// simulator passes its own, so a run's certificate dates are a function of the
// script rather than of the machine. Key and serial entropy stays on
// crypto/rand: Go's ECDSA key generation reseeds its DRBG from process-global
// state, so no injected reader makes key material reproducible anyway.
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
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/clock"
)

// caValidityYears is how many calendar years the generated CA is valid.
// Long-lived so users don't have to re-trust it often; it lives only on the
// local machine. Calendar years, not days: a year is not 365 days, so a
// 10*365*24h validity would fall short of ten years by two or three days
// depending on the leap days it spans, and the expiry is a date users reason
// about ("this CA is good until ..."), not a span of elapsed time.
const caValidityYears = 10

// caRenewBefore triggers regeneration when a loaded CA is within this window of
// expiry, so a stale on-disk CA is rotated rather than minting leaves that
// outlive their issuer.
const caRenewBefore = 30 * 24 * time.Hour

// leafValidity bounds minted leaf certs. Expired cached leaves are re-minted on
// demand (matters only for sessions longer than this).
const leafValidity = 24 * time.Hour

// certBackdate is how far before its NotBefore a minted certificate starts. The
// verifier's clock is not the minter's, so a certificate valid from exactly
// "now" can be rejected as not yet valid by a peer running a few seconds slow.
const certBackdate = time.Hour

// maxCacheEntries caps the per-host leaf cache so a long-running transparent
// proxy that sees many hosts can't grow it without bound. Eviction is
// approximate (drops an arbitrary entry), which is fine for a size guard.
const maxCacheEntries = 1024

// maxHostLen rejects hosts longer than the DNS name limit (RFC 1035, 253
// octets) before minting — no real SNI is longer, and it bounds the cost of an
// adversarial/huge host string.
const maxHostLen = 253

// CA is a local certificate authority that mints (and caches) per-host leaf
// certificates for TLS interception. Safe for concurrent use.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte // PEM of the CA cert, for trust installation

	clock clock.Clock

	mu    sync.Mutex
	cache map[string]*tls.Certificate // host → minted leaf
}

// Options are the sources a CA's output depends on, injected so a run's
// certificates are a function of the script rather than of the machine. The
// zero value is production: the host clock and crypto/rand.
type Options struct {
	// Clock dates every validity window and decides when a cached leaf expires
	// or a stored CA is stale. Nil means clock.SystemClock.
	Clock clock.Clock
}

// resolve fills the zero value with the production clock. Idempotent, and
// called by every constructor rather than only the exported one, so no path can
// reach a clock read with a nil source.
func (o Options) resolve() Options {
	if o.Clock == nil {
		o.Clock = clock.SystemClock{}
	}
	return o
}

// LoadOrCreateCA loads the CA key/cert from dir, generating and persisting a new
// one (0600 key, 0644 cert) if absent. dir is created if needed.
func LoadOrCreateCA(dir string) (*CA, error) {
	return LoadOrCreateCAWith(dir, Options{})
}

// LoadOrCreateCAWith is LoadOrCreateCA with the clock and entropy sources named
// explicitly. Callers that replay a run (tests, simulation) pass their own; the
// shipped command line does not and gets the zero Options.
func LoadOrCreateCAWith(dir string, opts Options) (*CA, error) {
	opts = opts.resolve()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mitm: create ca dir: %w", err)
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := parseCA(certPEM, keyPEM, opts)
		// Reuse only a parseable CA that isn't expired or about to expire;
		// otherwise fall through to regenerate (corrupt, or stale on disk).
		if err == nil && opts.Clock.Now().Before(ca.cert.NotAfter.Add(-caRenewBefore)) {
			// The CA key can decrypt every intercepted TLS session, so flag it if
			// permissions were loosened on disk after we wrote it 0600. Not on
			// Windows: Go reports 0666 for every writable file there, so the
			// check would fire on every run and the chmod it suggests only
			// toggles the read-only attribute.
			if runtime.GOOS != "windows" {
				if info, statErr := os.Stat(keyPath); statErr == nil && info.Mode().Perm()&0o077 != 0 {
					slog.Warn("mitm CA key has overly permissive permissions",
						"path", keyPath, "fix", "chmod 600 "+keyPath, "mode", info.Mode().Perm())
				}
			}
			return ca, nil
		}
		// Say why the on-disk CA was replaced: regenerating silently would make a
		// recurring "every agent re-trusts the CA" cycle look like a fresh install.
		switch {
		case err != nil:
			slog.Debug("mitm: existing CA is unreadable, regenerating", "err", err)
		case certErr != nil || keyErr != nil:
			// A read that failed (a missing half of the pair, a permission
			// change) also replaces the CA, invalidating the trust every agent
			// was told to install. Name the read error; it is the reason.
			slog.Warn("mitm: cannot read existing CA, regenerating",
				"cert", certPath, "cert_err", certErr, "key", keyPath, "key_err", keyErr)
		default:
			slog.Debug("mitm: existing CA is expiring, regenerating",
				"not_after", ca.cert.NotAfter.Format(time.RFC3339))
		}
	}

	ca, err := generateCA(opts)
	if err != nil {
		return nil, err
	}
	if err := persistCA(ca, certPath, keyPath); err != nil {
		return nil, err
	}
	return ca, nil
}

// persistCA writes the CA key (0600) and cert (0644) to disk.
func persistCA(ca *CA, certPath, keyPath string) error {
	keyOut, err := marshalKeyPEM(ca.key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		return fmt.Errorf("mitm: write ca key: %w", err)
	}
	// os.WriteFile only applies the mode when it creates the file; overwriting an
	// existing key (a pre-0600 build, or perms loosened on disk) keeps the old
	// permissions. The CA key decrypts every intercepted TLS session, so force
	// 0600 explicitly rather than trusting the prior state.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return fmt.Errorf("mitm: secure ca key perms: %w", err)
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0o644); err != nil {
		return fmt.Errorf("mitm: write ca cert: %w", err)
	}
	return nil
}

// generateCA creates a fresh self-signed CA in memory (not persisted).
func generateCA(opts Options) (*CA, error) {
	opts = opts.resolve()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	// One clock read for the whole validity window: NotBefore and NotAfter
	// derived from separate reads can disagree on which side of a clock
	// adjustment the certificate was minted.
	now := opts.Clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "muninn-sidecar local CA", Organization: []string{"muninn-sidecar"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              now.AddDate(caValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // can only sign leaves, not intermediate CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mitm: self-sign ca: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca: %w", err)
	}
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		clock:   opts.Clock,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

func parseCA(certPEM, keyPEM []byte, opts Options) (*CA, error) {
	opts = opts.resolve()
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("mitm: bad ca cert pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("mitm: bad ca key pem")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca key: %w", err)
	}
	// An interrupted persistCA (or two processes interleaving regeneration) can
	// leave a key that doesn't belong to the cert; reusing such a pair mints
	// leaves whose CA signature never verifies. Reject so the caller regenerates.
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("mitm: ca key does not match ca cert")
	}
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		clock:   opts.Clock,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

func marshalKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("mitm: marshal ca key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// CertPEM returns the CA certificate in PEM form, for trust installation
// (NODE_EXTRA_CA_CERTS / SSL_CERT_FILE in the child, or manual import).
func (c *CA) CertPEM() []byte { return c.certPEM }

// LeafFor returns a leaf certificate valid for host (the SNI server name),
// signed by the CA. Minted leaves are cached per host (bounded to
// maxCacheEntries) and re-minted once expired. Safe for concurrent use.
func (c *CA) LeafFor(host string) (*tls.Certificate, error) {
	host, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}
	// A DNS name can't exceed 253 octets (RFC 1035); reject implausibly long
	// hosts rather than minting a cert with a giant SAN — that wastes work
	// (ASN.1-encoding + signing a huge string) on input that can't be a real
	// SNI, and bounds a pathological slow path.
	if len(host) > maxHostLen {
		return nil, fmt.Errorf("mitm: host too long (%d > %d)", len(host), maxHostLen)
	}
	now := c.clock.Now()

	c.mu.Lock()
	if cached, ok := c.cache[host]; ok && cached.Leaf != nil && now.Before(cached.Leaf.NotAfter) {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	leaf, err := c.mintLeaf(host)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// Bound the cache (unless we're refreshing an existing host, which replaces
	// in place). The victim is the lexicographically smallest host, not whatever
	// the map range happens to visit first: Go randomizes map iteration order,
	// so an arbitrary victim leaves a different host cached from one run to the
	// next, and a replayed run then re-mints leaves for a different set of hosts
	// and reaches a different cache state. The scan is over a bounded cache.
	if _, exists := c.cache[host]; !exists && len(c.cache) >= maxCacheEntries {
		victim := ""
		for k := range c.cache {
			if victim == "" || k < victim {
				victim = k
			}
		}
		delete(c.cache, victim)
	}
	c.cache[host] = leaf
	c.mu.Unlock()
	return leaf, nil
}

func (c *CA) mintLeaf(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := c.clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-certBackdate),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	addSAN(tmpl, host)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mitm: sign leaf: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse freshly signed leaf for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw}, // leaf + CA so clients can chain
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitm: serial: %w", err)
	}
	return n, nil
}
