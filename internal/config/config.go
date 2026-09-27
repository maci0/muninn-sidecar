// Package config resolves MuninnDB connection settings from flags, environment
// variables, and defaults. Every binary in this repo (msc, msc-eval, msc-bench,
// msc-qa) resolves them here, so the default endpoint, the env var names, and
// the token-file location each have exactly one definition.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// DefaultMCPURL is the local MuninnDB endpoint used when neither the flag
	// nor MUNINN_MCP_URL is set.
	DefaultMCPURL = "http://127.0.0.1:8750/mcp"

	// DefaultVault is the last-resort vault name, used when neither the flag,
	// MSC_VAULT, nor the current directory yields a usable name.
	DefaultVault = "sidecar"
)

// tokenFile is the bearer-token file MuninnDB writes on first start, relative
// to the user's home directory.
const tokenFile = ".muninn/mcp.token"

// EnvOr returns the value of the environment variable key, or def when the
// variable is unset or empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvBool reports whether a switch variable is on. Unset and empty are off,
// and so are the explicit negatives (0, false, off, no, in any case): a shell
// profile that exports the switch as 0 must be able to turn it off, which
// "set means on" cannot express. Every other value is on, so a switch stays
// usable as a bare `MSC_WS_DEBUG=1` in a command prefix.
func EnvBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// MCPURL returns the MuninnDB MCP endpoint: the flag value if non-empty, else
// MUNINN_MCP_URL, else DefaultMCPURL.
func MCPURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return EnvOr("MUNINN_MCP_URL", DefaultMCPURL)
}

// OneOf rejects a configuration value that is outside a fixed set, so a typo
// names itself at startup instead of quietly selecting the default branch of a
// switch further down. Pass "" in allowed for an option where the empty value
// is itself meaningful; it is left out of the message's list and called out
// separately.
func OneOf(option, value string, allowed ...string) error {
	for _, a := range allowed {
		if a == value {
			return nil
		}
	}
	var shown []string
	for _, a := range allowed {
		if a != "" {
			shown = append(shown, a)
		}
	}
	empty := ""
	if len(shown) < len(allowed) {
		empty = " (or the empty value)"
	}
	return fmt.Errorf("invalid %s %q: must be one of %s%s", option, value, strings.Join(shown, ", "), empty)
}

// ArgSecretWarning returns the message to print when a secret was supplied as
// a command-line argument, or "" when there was nothing to warn about. On Unix
// the argument vector is world-readable through ps and the argument is kept in
// shell history, so a secret passed as a flag is exposed to every other user on
// the machine; envName names the variable that keeps it out of both. Callers
// pass the raw flag value: empty means the secret came from the environment or
// a file, where the warning does not apply.
func ArgSecretWarning(flagName, flagVal, envName string) string {
	if flagVal == "" {
		return ""
	}
	return fmt.Sprintf("%s passes a secret in the process list, readable by other users via ps and kept in shell history; set %s instead", flagName, envName)
}

// IsLoopbackHost reports whether host names the local machine, so traffic sent
// there never leaves it.
func IsLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// IsOpenAIHost reports whether host is an OpenAI API endpoint. Callers use it
// to warn before an OPENAI_API_KEY is sent to some other host.
func IsOpenAIHost(host string) bool {
	h := strings.ToLower(host)
	return h == "api.openai.com" || strings.HasSuffix(h, ".openai.com")
}

// ValidateURL rejects an endpoint msc could never dial (a missing scheme, a
// non-HTTP scheme, or no host), so a typo fails at startup with a clear message
// naming the option instead of surfacing later as a confusing transport error
// from the health check or the first request. An empty raw is accepted: it means
// "no endpoint configured", which the caller handles with its own error.
//
// option names the setting in the message ("MuninnDB URL", "--ground-url") so
// the user knows which value to fix.
func ValidateURL(option, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", option, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %q: scheme must be http or https", option, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s %q: missing host", option, raw)
	}
	return nil
}

// Token resolves the MuninnDB bearer token: the flag value if non-empty, else
// MUNINN_TOKEN, else the ~/.muninn/mcp.token file. Returns "" when none is set;
// a server that needs no auth is the only correct consumer of an empty token.
func Token(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if t := os.Getenv("MUNINN_TOKEN"); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, tokenFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Warn if the token file is readable by group or other users. Only where
	// those bits mean something: Windows has no group/other distinction, and Go
	// reports 0666 for every writable file there, so the check (and the chmod
	// it suggests, which only toggles the read-only attribute) is noise.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil {
			if info.Mode().Perm()&0o077 != 0 {
				slog.Warn("token file has overly permissive permissions",
					"path", path, "fix", "chmod 600 "+path, "mode", info.Mode().Perm())
			}
		}
	}
	return strings.TrimSpace(string(data))
}

// Vault returns the MuninnDB vault name: the flag value if non-empty, else
// MSC_VAULT, else the current directory's base name (so a session's memories
// follow the project it runs in), else DefaultVault.
func Vault(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("MSC_VAULT"); v != "" {
		return v
	}
	if cwd, err := os.Getwd(); err == nil {
		if name := dirName(cwd); name != "" {
			return name
		}
	}
	return DefaultVault
}

// dirName is the last element of a directory path, or "" when the path names
// no directory: a filesystem root ("/", "C:\", "\\host\share") or a relative
// step like "." Both separators count on every OS, so a Windows path handed to
// a Unix build (and the reverse) is still read as a path rather than a name.
// filepath.Base is not enough on its own: it returns "\" for "C:\", and only
// the platform that produced the path knows which separator is native.
func dirName(dir string) string {
	trimmed := strings.TrimRight(dir, `/\`)
	if !strings.ContainsAny(trimmed, `/\`) {
		return ""
	}
	return trimmed[strings.LastIndexAny(trimmed, `/\`)+1:]
}
