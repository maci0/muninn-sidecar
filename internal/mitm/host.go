package mitm

import (
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"
)

// normalizeHost canonicalizes a host for minting and for comparison: it strips
// any port, unwraps a bracketed IPv6 literal, lowercases, and drops the DNS
// root's trailing dot, so "API.OpenAI.com:443", "api.openai.com", and
// "api.openai.com." all name the same leaf.
//
// A non-ASCII host is an error. SNI and CONNECT carry A-labels (punycode), so a
// host with a non-ASCII byte is a client speaking a form no certificate can
// name — x509 refuses to encode it in a SAN ("cannot be encoded as an
// IA5String"), and comparing it against a punycode name never matches, so the
// host would be silently blind-tunneled. The Unicode spelling of an IDN has to
// be written in its punycode form ("xn--mnchen-3ya.de").
func normalizeHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1] // bracketed IPv6 with no port
	}
	// Trim again: unwrapping brackets / splitting can re-expose whitespace.
	host = strings.ToLower(strings.TrimSpace(host))
	// The DNS root's explicit trailing dot, trimmed again because dropping it
	// can expose whitespace that sat between the dot and the end of the name.
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if !isASCII(host) {
		return "", fmt.Errorf("mitm: non-ASCII host %q: name an IDN in its punycode form (xn--)", host)
	}
	return host, nil
}

// isASCII reports whether s contains no byte above the ASCII range.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// addSAN attaches the host to the certificate's Subject Alternative Names as
// either an IP SAN (when host is an IP literal) or a DNS SAN. A SAN is required —
// modern TLS clients ignore the CommonName.
func addSAN(tmpl *x509.Certificate, host string) {
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		return
	}
	tmpl.DNSNames = append(tmpl.DNSNames, host)
}
