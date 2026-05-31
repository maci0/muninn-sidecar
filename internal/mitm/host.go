package mitm

import (
	"crypto/x509"
	"net"
	"strings"
)

// normalizeHost lowercases a host and strips any port, so "API.OpenAI.com:443"
// and "api.openai.com" mint/cache the same leaf. IPv6 literals (in brackets) are
// unwrapped. A bare host with no port is returned lowercased.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1] // bracketed IPv6 with no port
	}
	// Trim again: unwrapping brackets / splitting can re-expose whitespace.
	return strings.ToLower(strings.TrimSpace(host))
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
