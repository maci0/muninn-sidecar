// Package redact scrubs well-known secret formats (API keys, tokens, private
// keys, sensitive key=value assignments) and directly-identifying personal data
// (email addresses, payment card numbers, US Social Security numbers) from text,
// replacing them with a [REDACTED] marker. It is
// shared by the store (scrub before persisting a captured exchange) and the
// injector (scrub recalled memory content before it is injected into an outgoing
// request — defense in depth against secrets/PII stored by other clients or
// before redaction existed).
//
// Patterns are deliberately conservative — anchored to distinctive provider
// prefixes/structures, sensitive key names, or the unambiguous email grammar —
// to avoid corrupting prose. This is not a substitute for never pasting secrets
// or personal data into an agent, but it stops the obvious leaks before captured
// conversations persist in long-term memory and resurface on recall.
package redact

import "regexp"

// Marker replaces matched secret material.
const Marker = "[REDACTED]"

var patterns = []*regexp.Regexp{
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
	// xAI (Grok) API keys.
	regexp.MustCompile(`xai-[A-Za-z0-9]{20,}`),
	// Google OAuth access tokens (gcloud / OAuth-mode agents such as agy).
	regexp.MustCompile(`ya29\.[0-9A-Za-z_-]{20,}`),
	// JSON Web Tokens (three base64url segments).
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// Bearer tokens in headers/prose.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
	// Stripe secret / restricted keys (live or test) — not pk_ (publishable).
	regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}`),
	// GitHub fine-grained personal access token.
	regexp.MustCompile(`github_pat_[0-9A-Za-z_]{60,}`),
	// npm access token.
	regexp.MustCompile(`npm_[0-9A-Za-z]{36}`),
	// HTTP Basic auth header (base64 user:pass).
	regexp.MustCompile(`(?i)authorization:\s*basic\s+[A-Za-z0-9+/]{16,}=*`),
	// Email addresses — directly-identifying personal data (GDPR/CCPA). The
	// grammar is distinctive (local@domain.tld) so false positives in prose are
	// rare. Redacting here keeps emails out of long-term memory where they would
	// persist and resurface on recall/injection.
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
	// Payment card numbers (PCI-DSS / GDPR financial data). Anchored to the
	// major-brand prefixes (Visa 4, Mastercard 51-55 / 2221-2720, Amex 34/37,
	// Discover 6011/65) so generic numeric IDs and version strings are not
	// caught. Covers the contiguous form.
	regexp.MustCompile(`\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|2[2-7][0-9]{14}|3[47][0-9]{13}|6(?:011|5[0-9]{2})[0-9]{12})\b`),
	// Payment card numbers with space/dash group separators (4-4-4-4), same
	// brand anchoring as above.
	regexp.MustCompile(`\b(?:4[0-9]{3}|5[1-5][0-9]{2}|2[2-7][0-9]{2}|6011|65[0-9]{2}|3[47][0-9]{2})[ -][0-9]{4}[ -][0-9]{4}[ -][0-9]{1,4}\b`),
	// US Social Security numbers (sensitive government identifier). Restricted
	// to the dashed 3-2-4 grouping, which is distinctive enough to avoid the
	// false positives a bare 9-digit run would cause against other IDs.
	regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`),
}

// kvPattern catches sensitive key=value / key: value assignments — the dominant
// real-world leak vector (a pasted .env file or shell export, where the value
// has no distinctive format so the key name is the only signal). It keeps the
// key and separator and redacts only the value. A 6-char minimum on the value
// avoids redacting trivial settings (token=1, password=). The key must be
// immediately followed by ':' or '=', so prose like "the secret to success" is
// untouched. The key may carry an identifier prefix (DB_PASSWORD, AWS_SECRET_KEY,
// OPENAI_API_KEY) since the sensitive word is often a suffix after '_'.
//
//	group 1: key  group 2: separator (+ optional quotes)  group 3: value
var kvPattern = regexp.MustCompile(
	`(?i)([A-Za-z0-9_.-]*(?:passwd|password|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|access[_-]?token|token|credentials?))(["']?\s*[:=]\s*["']?)([^\s"',;]{6,})`)

// Secrets replaces well-known credential formats in s with Marker. Returns s
// unchanged when it contains no recognized secret.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, re := range patterns {
		// ReplaceAllString allocates a full copy of s even when nothing matches.
		// Most content carries no secret, so gate on a non-allocating MatchString
		// first — the common path then makes zero allocations across all patterns.
		if re.MatchString(s) {
			s = re.ReplaceAllString(s, Marker)
		}
	}
	// Sensitive key=value assignments: redact the value, keep the key for context.
	if kvPattern.MatchString(s) {
		s = kvPattern.ReplaceAllString(s, "${1}${2}"+Marker)
	}
	return s
}
