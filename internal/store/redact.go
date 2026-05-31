package store

import "regexp"

// Secret redaction for captured content. Coding agents routinely carry API keys,
// tokens, and .env contents in their context; storing those verbatim in the
// long-term memory graph is a security risk (they persist and resurface via
// recall). Before a captured exchange is stored, well-known credential formats
// are replaced with a [REDACTED] marker.
//
// Patterns are deliberately conservative — anchored to distinctive provider
// prefixes or structures — to avoid corrupting legitimate prose. This trades
// recall of rare false positives for not persisting real secrets; for a memory
// store that's the right default. It is not a substitute for never pasting
// secrets into an agent, but it stops the obvious leaks.
var redactPatterns = []*regexp.Regexp{
	// PEM private key blocks (any type): redact the whole block.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	// OpenAI / Anthropic style keys: sk-, sk-ant-, sk-proj-, …
	regexp.MustCompile(`sk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`),
	// AWS access key ID.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// GitHub tokens (PAT / OAuth / refresh / server / user-to-server).
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
	// Google API key.
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	// Slack tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// JSON Web Tokens (three base64url segments).
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// Bearer tokens in headers/prose.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
}

const redactionMarker = "[REDACTED]"

// redactSecrets replaces well-known credential formats in s with a marker.
// Returns s unchanged when it contains no recognized secret.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, re := range redactPatterns {
		s = re.ReplaceAllString(s, redactionMarker)
	}
	return s
}
