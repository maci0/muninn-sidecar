package agents

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// CABundlePath is where writeCombinedCABundle puts the system-roots+CA bundle.
func CABundlePath(caCertPath string) string {
	return filepath.Join(filepath.Dir(caCertPath), "ca-bundle.pem")
}

// systemRootPaths are well-known CA bundle locations, probed in order.
// crypto/x509 keeps its equivalent list unexported, so it is mirrored here.
var systemRootPaths = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu/Arch
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora/RHEL
	"/etc/ssl/ca-bundle.pem",             // OpenSUSE
	"/etc/ssl/cert.pem",                  // Alpine/OpenBSD (macOS: present, but no certificates in it)
}

// systemRootsPEM returns the system root CA bundle, honoring an SSL_CERT_FILE
// already set by the user before probing the well-known locations. Returns nil
// if no bundle is found.
func systemRootsPEM() []byte {
	candidates := systemRootPaths
	if v := os.Getenv("SSL_CERT_FILE"); v != "" {
		candidates = append([]string{v}, candidates...)
	}
	return firstPEMRoots(candidates)
}

// firstPEMRoots returns the contents of the first candidate that is a readable
// PEM bundle, or nil when none is.
func firstPEMRoots(candidates []string) []byte {
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// A readable file is not a bundle. macOS's /etc/ssl/cert.pem exists but
		// holds only a pointer to the keychain, with no certificates in it;
		// accepting it would write a ca-bundle.pem holding nothing but msc's own
		// CA and point SSL_CERT_FILE at it, replacing the child's entire root
		// store with one certificate. Skip a candidate with no CERTIFICATE block
		// and keep probing, so a real bundle later in the list still wins.
		if !hasPEMCertificate(b) {
			continue
		}
		return b
	}
	return nil
}

// hasPEMCertificate reports whether b contains at least one PEM CERTIFICATE
// block. It reads the block headers only; parsing the certificates themselves
// would reject a bundle carrying one expired or malformed root, which is the
// child's business to report, not a reason to drop every root with it.
func hasPEMCertificate(b []byte) bool {
	for len(b) > 0 {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			return true
		}
	}
	return false
}

// HasSystemCABundle reports whether a system PEM root bundle was found to
// combine msc's CA with. False where the trusted roots live in an OS store
// rather than a PEM file (Windows, and macOS, whose /etc/ssl/cert.pem holds no
// certificates), and on a system with no bundle installed at all. The answer
// decides whether writeCombinedCABundle writes one
// and whether MITMOverrides sets the trust-store-replacing variables: ExecMITM
// leaves those variables unset when it has no system roots, so a dry-run
// preview asks here instead of writing a bundle. It is the same probe
// writeCombinedCABundle makes, so the preview and the real launch cannot
// disagree about it.
func HasSystemCABundle() bool {
	return systemRootsPEM() != nil
}

// writeCombinedCABundle writes the system root CAs followed by msc's CA into
// ca-bundle.pem beside caCertPath and returns the bundle's path. The bundle is
// for env vars that REPLACE the default trust store (SSL_CERT_FILE,
// REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE): pointing them at msc's CA alone would
// break TLS to every host msc blind-tunnels under --mitm-host scoping. With no
// system bundle to combine (Windows), there is nothing to write, so the empty
// path is returned and callers leave those variables unset.
func writeCombinedCABundle(caCertPath string) (string, error) {
	return writeCABundle(caCertPath, systemRootsPEM())
}

// writeCABundle is writeCombinedCABundle with the system roots already probed,
// so the no-roots branch is reachable without depending on what the host has
// installed.
func writeCABundle(caCertPath string, roots []byte) (string, error) {
	if len(roots) == 0 {
		return "", nil
	}
	ca, err := os.ReadFile(caCertPath)
	if err != nil {
		return "", fmt.Errorf("read CA cert: %w", err)
	}
	bundle := make([]byte, 0, len(roots)+1+len(ca))
	bundle = append(bundle, roots...)
	if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
		bundle = append(bundle, '\n')
	}
	bundle = append(bundle, ca...)
	bundlePath := CABundlePath(caCertPath)
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return "", fmt.Errorf("write CA bundle: %w", err)
	}
	return bundlePath, nil
}
