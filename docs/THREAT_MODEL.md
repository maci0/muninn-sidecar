# Threat model: muninn-sidecar (msc)

Last reviewed: 2026-09-27. Scope: the `msc` proxy path (`cmd/msc` plus the
`internal/` packages it wires). The eval/bench/qa CLIs (`cmd/msc-eval`,
`cmd/msc-bench`, `cmd/msc-qa`) are developer tools that read local datasets and
talk to a local MuninnDB; they share `internal/config` and `internal/mcpclient`
and are covered only where they differ materially.

No owner and no review cadence are recorded for this document. The repository
does not name one.

## Risk-ranked summary

Ranked by exploitability on a single-user developer machine, then by impact.

| # | Threat | Where | Exploitability | Impact | Mitigation today |
|---|--------|-------|----------------|--------|------------------|
| 1 | Any local process can use the proxy, and under `--mitm` can have its plaintext captured and stored | `internal/proxy/proxy.go:217`, `internal/proxy/mitm.go:62` | High (loopback, no credential of any kind) | Full conversation capture into the vault, plus use of msc as a local open relay | None. Loopback bind only |
| 2 | Vault content is a prompt-injection channel into the agent's system prompt | `internal/inject/format.go:55` | Medium (any writer to the vault: another msc session, another client, a poisoned corpus) | Agent behaviour driven by attacker text, at `system` priority | Relevance gate, fitness flags, dedup, budget. No content sanitization |
| 3 | Secrets and PII persist in long-term memory and are re-sent to the provider on recall | `internal/redact/redact.go:25`, `internal/store/muninn.go:250` | Medium (a secret whose format is not in the pattern list) | Credential disclosure to the LLM provider and to MuninnDB | Best-effort pattern redaction, on by default; always on the inject side |
| 4 | Bearer token for MuninnDB crosses the network in the clear on a non-loopback HTTP endpoint | `cmd/msc/main.go:189`, `internal/mcpclient/client.go:147` | Low (user-configured remote endpoint) | Full vault read/write for the token's lifetime | Warn only; `--mcp-url` scheme validated, TLS 1.3 floor for HTTPS |
| 5 | The MITM CA private key is a machine-wide decryption key for everything the child sends | `internal/mitm/ca.go:110` | Low (requires local file read as another user, or root) | Decryption of any session routed through msc, and minting leaves trusted by anything that imported `msc ca` | `0600` key in a `0700` dir, permission re-check and warn on every load |
| 6 | Unbounded concurrent buffering on a loopback listener with no connection cap | `internal/proxy/proxy.go:55`, `:59`, `:190` | Low (local, requires many concurrent clients) | msc memory exhaustion, agent outage | Per-body size caps, 30s `ReadHeaderTimeout` against slowloris, timeouts on every leg |
| 7 | Cross-project memory bleed through the default vault name | `internal/config/config.go:115` | Low (two projects with the same directory base name, or one shared checkout) | Content from one codebase injected into an agent working on another | None; `--vault` / `MSC_VAULT` override |

## 1. Attack surface inventory

Entry points, all of them local. Nothing in this repository listens on a
non-loopback address, and no third-party Go module is linked (`go.mod` has no
`require` block), so the dependency surface is the standard library plus
whatever the launched agent and MuninnDB themselves load.

| Entry point | Address | File |
|-------------|---------|------|
| HTTP reverse proxy | `127.0.0.1:0` (random port) | `cmd/msc/main.go:303`, `internal/proxy/proxy.go:217` |
| HTTPS `CONNECT` proxy (`--mitm`) | same listener | `internal/proxy/proxy.go:247`, `internal/proxy/mitm.go:29` |
| WebSocket upgrade splice + decoder | reached only through the CONNECT tunnel | `internal/proxy/mitm.go:150`, `internal/proxy/wscapture.go` |
| SSE / ndjson response tap | upstream response bodies | `internal/proxy/stream.go:104` |
| CLI flags and passthrough agent argv | `msc <agent> [args...]` | `cmd/msc/flags.go`, `internal/agents/agents.go:347` |
| Environment variables | `MUNINN_MCP_URL`, `MUNINN_TOKEN`, `MSC_VAULT`, `MSC_UPSTREAM_<AGENT>`, `MSC_WS_DEBUG`, `OPENAI_API_KEY`, `MUNINN_*` | `internal/config/config.go:41`, `:86`, `:115`, `internal/proxy/wscapture.go:24`, `internal/agents/agents.go:32` |
| Config files read from disk | `~/.muninn/mcp.token`, `~/.config/muninn-sidecar/mitm/{ca-key.pem,ca-cert.pem,ca-bundle.pem}` | `internal/config/config.go:97`, `internal/mitm/ca.go:74`, `internal/agents/agents.go:412` |
| Outbound JSON-RPC client to MuninnDB | `MUNINN_MCP_URL`, default `http://127.0.0.1:8750/mcp` | `internal/mcpclient/client.go:122` |
| Outbound HTTPS to the LLM provider | resolved upstream, TLS 1.3 floor | `internal/proxy/proxy.go:154` |
| Optional grounding judge (HTTP URL or local CLI) | `--ground-url`, `--ground-cmd` | `internal/grounding/grounding.go:154`, `:220` |
| Launched child process | agent binary resolved via `PATH` | `internal/agents/agents.go:433` |
| Scheduled jobs | none | — |

Treated as untrusted, correctly: the request body and response body from the
agent and the provider (`internal/proxy/proxy.go:275`, `:426`), the MuninnDB
JSON-RPC response (`internal/mcpclient/client.go:160`), the WebSocket frames
(`internal/proxy/wsframe.go`), and every recalled memory string
(`internal/inject/format.go:72`).

## 2. Trust boundaries

1. **Local process → proxy listener.** Any process on the machine can reach the
   loopback port. There is no authentication, no peer-credential check, and no
   token check; the proxy accepts any request. This is the widest boundary in
   the system and the one the docs do not describe.
2. **Agent → msc → LLM provider.** The agent's API key rides in an `Authorization`
   header through msc in cleartext on loopback, and msc deliberately adds no
   `X-Forwarded-For` so the upstream cannot see the interception
   (`internal/proxy/proxy.go:341`).
3. **msc → MuninnDB.** Bearer token in an `Authorization` header
   (`internal/mcpclient/client.go:147`). Plaintext unless the endpoint is
   `https` or loopback; a warning is emitted, not an error
   (`cmd/msc/main.go:189`).
4. **MuninnDB vault → agent prompt.** The sharpest logical boundary. Vault
   content written by any session or any client is read back and spliced into
   the system prompt as a `<retrieved-context>` block
   (`internal/inject/format.go:55`). Selection is relevance-based, not
   provenance-based.
5. **msc → child agent process.** The child inherits the full parent environment
   (`internal/agents/agents.go:257`) and gains, under `--mitm`, trust of msc's
   CA plus proxy env vars that redirect its whole HTTPS traffic
   (`internal/agents/agents.go:294`).
6. **Local disk → msc.** The MITM CA key and the MuninnDB token file are read
   from fixed paths in the user's home.
7. **CLI flag → subprocess.** `--ground-cmd` is split on whitespace and executed
   with the query and recalled passages on stdin, never argv
   (`internal/grounding/grounding.go:220`).

Privilege transitions: the agent's API key, the MuninnDB bearer token, and the
CA signing key are the three credentials msc holds in the request path. None is
ever persisted by msc except the CA key, which it generates.

## 3. Assets

- **Conversation content**: prompts, code, file contents, tool output. Held in
  MuninnDB indefinitely.
- **LLM provider API keys**: transit only, in the request header.
- **MuninnDB bearer token**: grants full read/write on the memory vault
  (`internal/config/config.go:97`).
- **MITM CA private key**: can mint a certificate for any host
  (`internal/mitm/ca.go:44`). Ten-year validity, renewed only within 30 days of
  expiry.
- **Availability of the agent session**: the proxy sits in the hot path, so
  msc crashing or stalling breaks the agent.
- **Reputation of the user's code**: captured content leaves the machine to the
  provider and, with `--no-redact`, is stored unscrubbed.

## 4. Threats per boundary

**Local process → proxy (STRIDE: spoofing, information disclosure, DoS, EoP).**
Spoofing is trivial: the proxy cannot tell the agent from any other local
client. Under `--mitm` with the default intercept-all scope
(`internal/proxy/proxy.go:139`), any local client presenting any SNI receives a
minted leaf and its decrypted traffic is captured and stored, so a local
process can deliberately write into the user's vault, including content shaped
to steer a later agent session (threat 2 above). Non-intercepted hosts are
blind-tunneled to any `host:port` the client names
(`internal/proxy/mitm.go:206`), which makes msc a general local TCP relay with a
30s dial timeout and no destination policy. DoS: no connection cap, no
per-client quota; each in-flight request can hold up to 50 MiB of buffered
body on the request and response legs (`internal/proxy/proxy.go:55`, `:59`).

**msc → MuninnDB (tampering, repudiation, information disclosure).** The
response body is size-capped at 10 MiB and JSON-RPC protocol-level errors are
checked rather than treated as success (`internal/mcpclient/client.go:160`,
`:185`), so a hostile or broken server cannot silently corrupt a write. What
msc does not do is authenticate the response body beyond "it was valid JSON":
a compromised MuninnDB can inject arbitrary text that is later injected into an
agent's system prompt. Over HTTP to a remote host, the bearer token and the
memory content are both on the wire in the clear.

**Vault → prompt (tampering, elevation of privilege).** Recalled memories are
placed at system-priority, above the user's own instructions. Controls: a
cosine confidence gate, dropping memories MuninnDB flags `archived`,
`cancelled`, or `untrusted`, near-duplicate removal, and a token budget
(`--inject-budget`, default 2048). There is no escaping, sandboxing, or
provenance check on the text itself, and no per-writer namespace. Redaction runs
on the injected text always (`internal/inject/format.go:72`) but redacts
credentials, not instructions.

**Agent → provider (information disclosure, repudiation).** msc reads and
rewrites the request body on every captured path, so it is a privileged
interceptor by construction. It is transparent on the wire (no added headers)
but not invisible: `--debug` logs paths and upstream URLs with the query string
replaced (`internal/proxy/proxy.go:639`), and `MSC_WS_DEBUG` logs WebSocket
message types and sizes, never content (`internal/proxy/wscapture.go:18`).

**Disk → msc (spoofing, information disclosure).** Both the token file and the
CA key are read from fixed paths; msc warns but continues when their
permissions are loose (`internal/config/config.go:104`,
`internal/mitm/ca.go:85`). A local user who can replace `ca-key.pem` with a
readable file, or who points `MUNINN_MCP_URL` at a host they control, captures
the token and the memory traffic.

**Flag → subprocess (elevation of privilege).** `--ground-cmd` executes an
arbitrary command from the operator's own command line, bounded by a timeout,
with the query and passages on stdin. The blast radius is the user's own shell
authority, so this is a documented capability rather than a boundary crossing
by an attacker.

## 5. Mitigations, mapped

| Control | Covers | File |
|---------|--------|------|
| Loopback bind on a random port | remote network access | `cmd/msc/main.go:303` |
| No `X-Forwarded-For`; no User-Agent change | upstream cannot fingerprint the interception | `internal/proxy/proxy.go:170`, `:341` |
| Request/response body caps (50 MiB), stream line cap (1 MiB), text accumulation cap (16 KiB), MCP response cap (10 MiB) | memory exhaustion, gzip bomb | `internal/proxy/proxy.go:42`, `:45`, `:50`, `:55`, `:59`, `internal/mcpclient/client.go:21` |
| `ReadHeaderTimeout` 30s on both servers | slowloris | `internal/proxy/proxy.go:190`, `internal/proxy/mitm.go:122` |
| Bounded tunnel dial (30s) | hung CONNECT goroutine | `internal/proxy/mitm.go:20` |
| Leaf certificate cache capped at 1024 entries; host name length capped at 253 | unbounded CA memory, adversarial SNI | `internal/mitm/ca.go:44`, `:52` |
| TLS 1.3 floor to the real upstream, normal certificate verification | msc never forges trust toward the provider | `internal/proxy/proxy.go:163` |
| TLS 1.3 floor for HTTPS MuninnDB; `--mcp-url` scheme and host validated at startup | downgrade, undialable config | `internal/mcpclient/client.go:43`, `internal/config/config.go:66` |
| CA key `0600` in a `0700` directory, `chmod` forced on rewrite, permission re-check on every load | CA theft by another local user | `internal/mitm/ca.go:71`, `:85`, `:110` |
| Combined system-roots + CA bundle for the vars that replace the trust store | breaking TLS to blind-tunneled hosts | `internal/agents/agents.go:412` |
| Secret and PII redaction before storage, on by default, `--no-redact` warns loudly | credential persistence | `internal/redact/redact.go:25`, `cmd/msc/main.go:196` |
| Redaction always on the inject side, even with `--no-redact` | re-transmitting a secret stored by another client | `internal/inject/format.go:72` |
| Anti-recursion filters strip injected markers, `muninn_*` tool traffic, system reminders | a memory of a memory | `internal/proxy/filter.go:37` |
| Fuzz targets on the agent-facing parsers (proxy, apiformat, mcpclient, store) | parser crashes on hostile input | `internal/proxy/fuzz_test.go`, `internal/apiformat/fuzz_test.go`, `internal/mcpclient/fuzz_test.go`, `internal/store/fuzz_test.go` |
| `govulncheck ./...` in CI; no third-party modules | known vulnerabilities | `.github/workflows/ci.yml:135`, `go.mod` |
| Bounded async queue (256) and 8s drain | capture loss under store outage, unbounded growth | `internal/store/muninn.go:63`, `:96` |
| Sanitized error responses; query strings redacted in logs | stack traces and API keys in logs and client bodies | `internal/proxy/proxy.go:487`, `:639` |

No mitigation, by threat: the local listener accepts unauthenticated clients
(risk 1); vault content is not treated as untrusted instruction text (risk 2);
redaction is pattern-based and cannot be complete (risk 3); the plaintext-HTTP
token path is a warning, not a refusal (risk 4); the default vault name is
directory-derived with no project scoping (risk 7).

Single points of failure worth naming: the redaction pattern set is the only
control between captured content and the store, and the relevance gate is the
only control between the store and the agent's system prompt. Neither has a
second layer.

## 6. Abuse cases

These are scenarios with the enabling code path named. None was attempted.

1. **Local scrape into the vault.** A process started by the user, or by a
   malicious dev dependency, connects to the proxy port, sends a request on a
   capture path (`internal/proxy/proxy.go:319`), and its body is stored in the
   user's vault (`internal/proxy/proxy.go:462`). Under `--mitm` the same process
   can additionally ask for any host's tunnel and have its decrypted body
   captured (`internal/proxy/mitm.go:71`).
2. **Poisoned memory.** A planted memory that is semantically close to a future
   query passes the cosine gate and is injected at system priority
   (`internal/inject/format.go:55`). Nothing records which session or process
   wrote it; the vault is keyed by name only
   (`internal/config/config.go:115`).
3. **Cross-project bleed.** Two checkouts whose directories share a base name
   (for example two `src` trees) resolve to the same vault, so decisions made
   in one are injected into the other.
4. **Resource exhaustion.** Concurrent large bodies on the loopback listener
   consume up to 100 MiB of buffers per in-flight exchange with no connection
   cap (`internal/proxy/proxy.go:55`, `:59`).
5. **Relay abuse.** With `--mitm`, a local client gets a TCP tunnel to any
   reachable `host:port` (`internal/proxy/mitm.go:206`), usable to reach
   services bound to loopback that the client could otherwise reach only as the
   same user.
6. **Over-collection.** `--mitm` without `--mitm-host` decrypts every host the
   agent connects to (`internal/proxy/proxy.go:139`), including hosts unrelated
   to the LLM API. `--mitm-host` narrows this and blind-tunnels the rest.

## 7. SECURITY.md accuracy

Checked on 2026-09-27. The claims in [SECURITY.md](../SECURITY.md) about the
CA key mode, trust scoping to the child, redaction being best-effort and
on by default, the token file location, the loopback listener, the
standard-library-only build, and `govulncheck` in CI all match the code at the
references given in the mitigation table above. Two facts were undocumented and
have been added there: the listener is unauthenticated, and vault content is
injected into the agent's system prompt.

## 8. Response readiness

Recorded as-is; no owner or process is invented here.

- **Audit trail.** msc logs to stderr through `slog`, WARN by default, with
  `--log-json` for aggregation. Upstream 4xx/5xx responses are logged with
  status, method, path, agent, and duration
  (`internal/proxy/proxy.go:382`). There is no log of what was captured or
  stored: no exchange ids, no vault writes, no record of an injected block, and
  no record that a memory was selected or dropped. After an incident, the only
  available evidence is the MuninnDB content itself.
- **No record of local listener clients.** Requests are logged with method and
  path only, and nothing distinguishes the launched agent from any other local
  client, so misuse of the loopback port (risk 1) is not detectable after the
  fact.
- **Disclosure path.** [SECURITY.md](../SECURITY.md) points at GitHub private
  vulnerability reporting. The document states the reporting channel but not
  who triages it, what the response target is, or what a reporter should
  expect after acknowledgement.
