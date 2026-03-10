## CLI Audit Report: `msc`

### Issue 1: Inconsistent Flag Parsing for Non-Internal Commands
*   **Severity:** Major
*   **Location:** `cmd/msc/main.go` (`parseFlags`)
*   **Problem:** While `parseFlags` was updated to handle flags after internal subcommands (like `msc list --json`), it still deliberately stops parsing at the first non-flag argument if it's an external agent (like `msc claude --quiet`). If a user types `msc claude --quiet`, the `--quiet` flag is passed to the agent rather than being consumed by `msc`. To pass `--quiet` to `msc`, the user must type `msc --quiet claude`.
*   **Why it matters:** This breaks standard CLI conventions where global flags can appear anywhere before the "--" separator (e.g., `docker run --rm nginx`). It requires users to memorize a specific strict ordering rule (global flags *must* precede the agent name) which increases cognitive load.
*   **Recommended fix:** The flag parser should consume all known `msc` flags regardless of where they appear, until it hits the explicit `--` separator. Anything that is *not* a known `msc` flag, or anything after `--`, should be passed to the agent.
*   **Example improvement:** Allow `msc claude --quiet` to mean "run claude and make msc quiet". To pass `--quiet` explicitly to claude, the user would type `msc claude -- --quiet`.

### Issue 2: `--dry-run` Output Format
*   **Severity:** Suggestion
*   **Location:** `cmd/msc/main.go`
*   **Problem:** The `--dry-run` output is a custom plaintext block. Since `list` and `status` support `--json`, `--dry-run` could also support `--json` to allow programmatic inspection of what *would* be executed.
*   **Why it matters:** Improves machine-readability of configuration resolution.
*   **Recommended fix:** Update the `--dry-run` output block to respect `o.asJSON`.

## Global CLI Design Recommendations

1.  **Robust Flag Parsing:** The custom flag parser `parseFlags` is becoming a liability as the CLI grows. Hand-rolling a parser that handles `--flag=value`, `-d`, unknown flags, positional arguments, and `--` separators is prone to edge cases. The codebase would strongly benefit from migrating to `spf13/pflag` (which supports POSIX standard flags, including intermingling flags and arguments) or `urfave/cli/v2`.
2.  **Configuration File Support:** Support for reading default configurations (like `mcp-url` and `vault`) from a `~/.config/msc/config.yaml` would eliminate the need to set environment variables or pass flags constantly.
