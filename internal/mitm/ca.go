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
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// caValidity is how long the generated CA is valid. Long-lived so users don't
// have to re-trust it often; it lives only on the local machine.
const caValidity = 10 * 365 * 24 * time.Hour

// leafValidity bounds minted leaf certs. Short-ish, but the cache is per-process
// so this only matters for very long-running sessions.
const leafValidity = 365 * 24 * time.Hour

// CA is a local certificate authority that mints (and caches) per-host leaf
// certificates for TLS interception. Safe for concurrent use.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte // PEM of the CA cert, for trust installation

	mu    sync.Mutex
	cache map[string]*tls.Certificate // host → minted leaf
}

// LoadOrCreateCA loads the CA key/cert from dir, generating and persisting a new
// one (0600 key, 0644 cert) if absent. dir is created if needed.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mitm: create ca dir: %w", err)
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err == nil {
			return ca, nil
		}
		// Fall through to regenerate on a corrupt/unreadable pair.
	}

	ca, err := generateCA()
	if err != nil {
		return nil, err
	}
	keyOut, err := marshalKeyPEM(ca.key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		return nil, fmt.Errorf("mitm: write ca key: %w", err)
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("mitm: write ca cert: %w", err)
	}
	return ca, nil
}

// generateCA creates a fresh self-signed CA in memory (not persisted).
func generateCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "muninn-sidecar local CA", Organization: []string{"muninn-sidecar"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
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
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
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
	return &CA{cert: cert, key: key, certPEM: certPEM, cache: make(map[string]*tls.Certificate)}, nil
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
// signed by the CA. Minted leaves are cached per host for the process lifetime.
func (c *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	c.mu.Lock()
	if cached, ok := c.cache[host]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	leaf, err := c.mintLeaf(host)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
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
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	addSAN(tmpl, host)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mitm: sign leaf: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw}, // leaf + CA so clients can chain
		PrivateKey:  key,
		Leaf:        mustParse(der),
	}, nil
}

func mustParse(der []byte) *x509.Certificate {
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitm: serial: %w", err)
	}
	return n, nil
}
