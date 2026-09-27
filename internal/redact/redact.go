// Package redact scrubs well-known secret formats (API keys, tokens, private
// keys, sensitive key=value assignments) and directly-identifying personal data
// (email addresses, payment card numbers, US Social Security numbers, phone
// numbers, the caller's home-directory path) from text,
// replacing them with a [REDACTED] marker, and drops a URL password for
// display. It is
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
//
// Every call goes through Secrets, which is also where the home-directory
// rewrite lives. Home redaction needs the process's own home directory, so
// threading it through each call site would be one more thing a future caller
// can forget; a forgotten call site is an unredacted leak, not a cosmetic gap.
package redact

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Marker replaces matched secret material.
const Marker = "[REDACTED]"

// HomeMarker replaces the caller's home-directory prefix. Distinct from Marker
// so a reader can tell "a secret was here" from "this path was relative to the
// user's home", and deliberately not a real path so nothing resolves to it.
const HomeMarker = "[HOME]"

// rule pairs a redaction pattern with a necessary condition for it to match.
// Secrets runs the condition first (a substring or single byte-class scan) and
// only pays for the regex when it holds. Almost all the patterns start with a
// character class or a word boundary rather than a literal, so the regexp engine
// cannot use its literal-prefix fast path and falls back to a full scan of the
// input for each one: on a 4 KiB turn the un-gated list cost ~2.3ms, of which
// ~2.1ms was scans that a necessary-condition check rejects in a few
// microseconds. Every condition below is required by the pattern, so gating
// cannot change which spans are redacted.
type rule struct {
	re *regexp.Regexp
	// lit is a case-sensitive substring the match must contain.
	lit string
	// folds are case-insensitive substrings; the match must contain at least one.
	folds []string
	// numeric requires the input to look numeric at all (see numericCandidate):
	// a run of at least three digits, or a '+' / '(' that the international and
	// parenthesized phone forms anchor on. Shared by every card, SSN and phone
	// pattern, so Secrets evaluates it once per call rather than per rule.
	numeric bool
	// keyStem requires one of the sensitive key=value stems ("pass", "secret",
	// "key", "token", "credential"); every alternative in kvPattern contains one.
	keyStem bool
}

var patterns = []rule{
	// PEM private key blocks (any type): redact the whole block.
	{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		lit: "-----BEGIN "},
	// OpenAI / Anthropic style keys: sk-, sk-ant-, sk-proj-, … The \b keeps
	// hyphenated identifiers ("task-management-…") from matching mid-word.
	{re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`),
		lit: "sk-"},
	// AWS access key ID.
	{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	// GitHub tokens (PAT / OAuth / refresh / server / user-to-server).
	{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	// Google API key.
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	// Slack tokens.
	{re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	// xAI (Grok) API keys. \b as for sk- above.
	{re: regexp.MustCompile(`\bxai-[A-Za-z0-9]{20,}`),
		lit: "xai-"},
	// Google OAuth access tokens (gcloud / OAuth-mode agents such as agy).
	{re: regexp.MustCompile(`ya29\.[0-9A-Za-z_-]{20,}`)},
	// JSON Web Tokens (three base64url segments).
	{re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	// Bearer tokens in headers/prose.
	{re: regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
		folds: []string{"bearer"}},
	// Stripe secret / restricted keys (live or test) — not pk_ (publishable).
	{re: regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}`),
		folds: []string{"live", "test"}},
	// GitHub fine-grained personal access token.
	{re: regexp.MustCompile(`github_pat_[0-9A-Za-z_]{60,}`)},
	// npm access token.
	{re: regexp.MustCompile(`npm_[0-9A-Za-z]{36}`)},
	// HTTP Basic auth header (base64 user:pass).
	{re: regexp.MustCompile(`(?i)authorization:\s*basic\s+[A-Za-z0-9+/]{16,}=*`),
		folds: []string{"authorization:"}},
	// Payment card numbers (PCI-DSS / GDPR financial data). Anchored to the
	// major-brand prefixes (Visa 4, Mastercard 51-55 / 2221-2720, Amex 34/37,
	// Discover 6011/65) so generic numeric IDs and version strings are not
	// caught. Covers the contiguous form.
	{re: regexp.MustCompile(`\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|2[2-7][0-9]{14}|3[47][0-9]{13}|6(?:011|5[0-9]{2})[0-9]{12})\b`),
		numeric: true},
	// Payment card numbers with space/dash group separators (4-4-4-4), same
	// brand anchoring as above.
	{re: regexp.MustCompile(`\b(?:4[0-9]{3}|5[1-5][0-9]{2}|2[2-7][0-9]{2}|6011|65[0-9]{2}|3[47][0-9]{2})[ -][0-9]{4}[ -][0-9]{4}[ -][0-9]{1,4}\b`),
		numeric: true},
	// US Social Security numbers (sensitive government identifier). Restricted
	// to the dashed 3-2-4 grouping, which is distinctive enough to avoid
	// the false positives a bare 9-digit run would cause against other IDs.
	{re: regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`),
		numeric: true},
	// Phone numbers (direct identifier: GDPR Art. 4(1), CCPA §1798.140(v)).
	// A phone number is one of the most common directly-identifying values
	// pasted into a coding-agent session, and it survives every pattern above,
	// so without these it persists in long-term memory and is re-injected into
	// later provider requests.
	//
	// The NANP forms require the NPA and NXX to begin with 2-9 (NANP rules) and
	// a space/dash group separator to be present. Those two constraints are
	// what keep version strings, ISO dates, bare numeric IDs, and IPv4
	// addresses out: "1.2.3.4", "2026-09-27", and "192.168.100.100" all fail
	// on the leading digit or on the grouping requirement. A phone written as
	// one unbroken 10-digit run is deliberately left alone, being
	// indistinguishable from the account and order numbers it would corrupt.
	// The parenthesized form carries no leading \b because '(' is itself the
	// delimiter.
	//
	// Added last so the card patterns above win on any overlap.
	{re: regexp.MustCompile(`\+\d{8,15}\b`),
		numeric: true},
	{re: regexp.MustCompile(`\+\d{1,3}[ -](?:\d{1,4}[ -]){1,3}\d{2,4}\b`),
		numeric: true},
	{re: regexp.MustCompile(`\+\d{1,3}[ -]\([2-9][0-9]{2}\)[ -][2-9][0-9]{2}[ -][0-9]{4}\b`),
		numeric: true},
	{re: regexp.MustCompile(`\b[2-9][0-9]{2}[ -][2-9][0-9]{2}[ -][0-9]{4}\b`),
		numeric: true},
	{re: regexp.MustCompile(`\([2-9][0-9]{2}\)[ -][2-9][0-9]{2}[ -][0-9]{4}\b`),
		numeric: true},
}

// matches reports whether the input satisfies the rule's necessary condition.
// A rule with no condition always matches. The numeric condition is not checked
// here: it is shared by eight rules and evaluated once per Secrets call.
func (r rule) matches(s string) bool {
	if r.lit != "" && !strings.Contains(s, r.lit) {
		return false
	}
	if len(r.folds) > 0 {
		ok := false
		for _, f := range r.folds {
			if containsFold(s, f) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if r.keyStem && !containsAnyStem(s) {
		return false
	}
	return true
}

// minNumericRun is the shortest digit run any numeric pattern can match: the
// SSN and NANP phone forms both require three consecutive digits before their
// first separator.
const minNumericRun = 3

// numericCandidate reports whether s could contain a card number, SSN or phone
// number at all, in one pass. Every numeric pattern requires either a digit run
// of at least minNumericRun (cards 12-16, SSN 3-2-4, NANP phones 3-3-4, the
// +CC form 8-15) or one of the international / parenthesized anchors '+' and '(',
// which the looser phone grammars allow with only two trailing digits. Prose
// full of incidental single digits ("rotate every 2 hours", "in 2024") therefore
// skips all eight numeric patterns.
func numericCandidate(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			run++
			if run >= minNumericRun {
				return true
			}
			continue
		}
		if c == '+' || c == '(' {
			return true
		}
		run = 0
	}
	return false
}

// containsAnyStem reports whether s contains any sensitive key=value stem
// ("pass", "secret", "key", "token", "credential") — one is present in every
// kvPattern alternative, so without one no assignment can match, which skips the
// single most expensive scan in this package. The check is one pass, case-folded
// against the first letter only: non-ASCII bytes fold to themselves under |0x20
// and therefore never equal an ASCII stem letter, so they are skipped naturally.
func containsAnyStem(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] | 0x20 {
		case 'p':
			if matchFold(s[i:], "pass") {
				return true
			}
		case 's':
			if matchFold(s[i:], "secret") {
				return true
			}
		case 'k':
			if matchFold(s[i:], "key") {
				return true
			}
		case 't':
			if matchFold(s[i:], "token") {
				return true
			}
		case 'c':
			if matchFold(s[i:], "credential") {
				return true
			}
		}
	}
	return false
}

// containsFold reports whether s contains sub, ignoring ASCII case. sub must
// already be lowercased. Used as a necessary-condition gate, so an ASCII-only
// fold is sufficient: every sub in this package is ASCII, and a non-ASCII byte
// can only EqualFold to another non-ASCII byte.
func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	first := sub[0]
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i]|0x20 == first && matchFold(s[i:], sub) {
			return true
		}
	}
	return false
}

// matchFold reports whether s starts with sub, ignoring ASCII case. sub must
// already be lowercased.
func matchFold(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i < len(sub); i++ {
		if s[i]|0x20 != sub[i] {
			return false
		}
	}
	return true
}

// emailPattern catches email addresses, directly-identifying personal data
// (GDPR/CCPA). The grammar is distinctive (local@domain.tld) so false positives
// in prose are rare. Redacting here keeps emails out of long-term memory where
// they would persist and resurface on recall/injection. The optional :tail
// pulls in whatever follows a colon so Secrets can classify it: scp-style
// remotes (git@github.com:org/repo.git, deploy@host.tld:/var/www) share the
// email grammar but are addresses of machines, not people, and are kept; any
// other tail (email:password combo dumps, URL-embedded credentials) is redacted
// together with the email. The base grammar admits no ':', so a colon in the
// match always means the tail fired.
//
// The local part and every domain label are Unicode classes, not [A-Za-z0-9]:
// an ASCII-only grammar matches none of "user@почта.рф", "ünïcödé@dömain.de",
// or "josé@例え.jp", so every internationalized address a user pastes into a
// session would persist unredacted in long-term memory. \p{M} is carried in the
// local part and labels so a decomposed (NFD) spelling — what macOS filesystems
// and many IMAP servers hand out — still matches as one address instead of
// breaking at the combining accent. Each label must start with a letter or
// digit, which keeps the pattern from matching a bare "@" or a trailing
// punctuation dot. A punycode TLD (xn--p1ai) is ASCII and already matched; the
// display form of the same domain is what the classes above now cover.
var emailPattern = regexp.MustCompile(
	`[\p{L}\p{N}][\p{L}\p{N}\p{M}._%+\-]*@` +
		`[\p{L}\p{N}\p{M}][\p{L}\p{N}\p{M}\-]*(?:\.[\p{L}\p{N}\p{M}][\p{L}\p{N}\p{M}\-]*)*` +
		`\.[\p{L}]{2,}(?::\S+)?`)

// kvPattern catches sensitive key=value / key: value assignments — the dominant
// real-world leak vector (a pasted .env file or shell export, where the value
// has no distinctive format so the key name is the only signal). It keeps the
// key and separator and redacts only the value. A 6-char minimum on the value
// avoids redacting trivial settings (token=1, password=). The key must be
// immediately followed by ':' or '=', so prose like "the secret to success" is
// untouched. The key may carry an identifier prefix (DB_PASSWORD, AWS_SECRET_KEY,
// OPENAI_API_KEY) since the sensitive word is often a suffix after '_'. A
// quoted value is matched whole, quotes included and spaces allowed, up to the
// closing quote on the same line, so multi-word passphrases
// (PASSWORD="correct horse battery") do not leak past the first word. A quote
// left unterminated (truncated paste) still redacts via the last alternative,
// which accepts an opening quote followed by a bare token; failing open there
// would persist the secret.
//
//	group 1: key  group 2: separator (+ optional quote before it)  group 3: value
var kvPattern = regexp.MustCompile(
	`(?i)([A-Za-z0-9_.-]*(?:passwd|password|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|access[_-]?token|token|credentials?))(["']?\s*[:=]\s*)("[^"\n]{6,}"|'[^'\n]{6,}'|["'][^\s"',;]{6,}|[^\s"',;]{6,})`)

// kvRule gates kvPattern on the key stems every one of its alternatives carries.
var kvRule = rule{re: kvPattern, keyStem: true}

// emailRule gates emailPattern on '@', which its grammar requires.
var emailRule = rule{re: emailPattern, lit: "@"}

// compileHome builds the home-directory matcher for dir. It returns a nil
// pattern when dir cannot identify anyone: a root home would rewrite every
// absolute path in the text and destroy the content it is meant to protect, and
// an empty or relative value matches text that is not a path at all.
//
// The (?m) flag is load-bearing: a path is far more often the last thing on its
// line than the last thing in the text, and without it a bare home directory
// ("cd /home/alice" at the end of a captured turn) sails straight through.
func compileHome(dir string) (*regexp.Regexp, string) {
	home := strings.TrimRight(dir, `/\`)
	if home == "" || home == "." {
		return nil, ""
	}
	// The separator-or-end anchor is what keeps it from firing on a longer
	// directory that merely starts with the same characters: with home /home/al,
	// "/home/alpha/src" must survive untouched.
	return regexp.MustCompile(`(?m)` + regexp.QuoteMeta(home) + `(?:[\\/]|$)`), home
}

// homePattern matches the caller's home directory when it appears as a path
// prefix.
//
// The home directory is the single most reliable direct identifier in a coding
// session — every absolute path a tool prints, every stack frame, every
// "cd ~/..." a model writes carries it, and on macOS and Windows the leaf is the
// account's real name. No format pattern can reach it (there is nothing
// distinctive about "/home/alice"), and once a captured turn is written to
// long-term memory the path outlives the session and is re-sent to the provider
// on every later recall. Only the prefix goes: the remainder of the path is the
// part that carries the project's meaning and is kept.
var homePattern = sync.OnceValues(resolveHomePattern)

// resolveHomePattern is homePattern's initializer, named so a test can drive it
// against a home directory the process cannot resolve.
func resolveHomePattern() (*regexp.Regexp, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ""
	}
	return compileHome(home)
}

// redactHome rewrites the caller's home-directory prefix in s to HomeMarker,
// keeping the path that follows it.
func redactHome(s string) string {
	re, home := homePattern()
	if re == nil {
		return s
	}
	return re.ReplaceAllStringFunc(s, func(m string) string {
		return HomeMarker + strings.TrimPrefix(m, home)
	})
}

// URL returns raw with any password in its userinfo removed, so an endpoint
// carrying credentials (https://user:pass@host/mcp) is safe to put in a log
// line or on a terminal. The scheme, host, and path stay: those are what make
// the line diagnosable. Only the display form is redacted; the caller still
// dials the original.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}

// Secrets replaces well-known credential formats in s with Marker, and the
// caller's home-directory prefix with HomeMarker. Returns s unchanged when it
// contains no recognized secret.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	numeric := numericCandidate(s)
	for _, p := range patterns {
		// The numeric condition is evaluated once and shared: a replacement only
		// ever inserts Marker ("[REDACTED]"), which carries no digit and no '+'
		// or '(', so a false verdict can never become true later in the loop.
		if p.numeric && !numeric {
			continue
		}
		if !p.matches(s) {
			continue
		}
		// ReplaceAllString allocates a full copy of s even when nothing matches.
		// Most content carries no secret, so gate on a non-allocating MatchString
		// first — the common path then makes zero allocations across all patterns.
		if p.re.MatchString(s) {
			s = p.re.ReplaceAllString(s, Marker)
		}
	}
	// Email addresses: redact, but leave scp-style remotes intact. Only a tail
	// that looks like a remote path (has a '/', no '@') is exempt; any other
	// colon tail (email:password combos, URL-embedded credentials) is secret
	// material and is redacted along with the email.
	if emailRule.matches(s) && emailPattern.MatchString(s) {
		s = redactEmails(s)
	}
	// Sensitive key=value assignments: redact the value, keep the key for context.
	// A value already reduced to the marker (plus stray trailing bytes a quoted
	// match left behind) is skipped so redaction is idempotent.
	if kvRule.matches(s) && kvPattern.MatchString(s) {
		s = kvPattern.ReplaceAllStringFunc(s, func(m string) string {
			sub := kvPattern.FindStringSubmatch(m)
			if strings.HasPrefix(strings.TrimLeft(sub[3], `"'`), Marker) {
				return m
			}
			return sub[1] + sub[2] + Marker
		})
	}
	// Home directory last, on the text every pass above has already finished
	// with, so it sees the exact bytes that would otherwise be stored.
	return redactHome(s)
}

// redactEmails replaces every emailPattern match with Marker, except scp-style
// remotes (user@host:path), which address a machine rather than a person.
//
// A match that starts immediately after a colon is never a remote: it is the
// userinfo of a URL credential (scheme://user:password@host:port/path), where
// the grammar reads the password as the local part and the host as the domain.
// Redacting such a match removes the host while leaving the password in place,
// and its own colon tail is the port, so the remote exemption (which fires on
// any tail with a '/' and no '@') would keep the whole credential verbatim. So
// the ':' check comes first, and the credential is redacted as a unit.
func redactEmails(s string) string {
	matches := emailPattern.FindAllStringIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		hit := s[m[0]:m[1]]
		exempt := false
		if m[0] == 0 || s[m[0]-1] != ':' {
			if i := strings.IndexByte(hit, ':'); i >= 0 {
				tail := hit[i+1:]
				exempt = strings.Contains(tail, "/") && !strings.Contains(tail, "@")
			}
		}
		b.WriteString(s[last:m[0]])
		if exempt {
			b.WriteString(hit) // user@host:path remote, not an email
		} else {
			b.WriteString(Marker)
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
