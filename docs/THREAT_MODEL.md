# Threat model: muninn-sidecar (msc)

Last reviewed: 2026-09-27. Scope: the `msc` proxy path (`cmd/msc` plus the
`internal/` packages it wires), and the eval/bench/qa CLIs (`cmd/msc-eval`,
`cmd/msc-bench`, `cmd/msc-qa`) as far as they differ from it. Those three are
developer tools that read local dataset files and talk to a MuninnDB, but each
also dials an operator-named model or judge endpoint and can exec a
`--ground-cmd` / `--rewrite-cmd` / `--model-cmd`, so their own surface is
enumerated in section 1 rather than waved away. They share
`internal/config`, `internal/mcpclient`, `internal/grounding`, and
`internal/store` with `msc`, so a change to a shared package moves all four.

No owner and no review cadence are recorded for this document. The repository
does not name one.

## Risk-ranked summary

Ranked by exploitability on a single-user developer machine, then by impact.

| # | Threat | Where | Exploitability | Impact | Mitigation today |
|---|--------|-------|----------------|--------|------------------|
| 1 | Any local process can use the proxy listener, read its status endpoint, and under `--mitm` have its plaintext captured and stored | `internal/proxy/proxy.go:344`, `internal/proxy/mitm.go:59`, `internal/proxy/proxy.go:379` | High (loopback, no credential of any kind) | Full conversation capture into the vault, session state disclosure via `/__msc/health`, use of msc as a local open relay | None. Loopback bind only |
| 2 | Vault content is a prompt-injection channel into the agent's system prompt | `internal/inject/inject.go:491`, `internal/inject/format.go:160` | Medium (any writer to the vault: another msc session, another client, a poisoned corpus) | Agent behaviour driven by attacker text, at `system` priority | Relevance gate, fitness flags, dedup, budget. No content sanitization |
| 3 | Secrets and PII persist in long-term memory and are re-sent to the provider on recall | `internal/redact/redact.go:372`, `internal/store/muninn.go:471` | Medium (a secret whose format is not in the pattern list) | Credential disclosure to the LLM provider and to MuninnDB | Best-effort pattern redaction, on by default; always on the inject side |
| 4 | Bearer token for MuninnDB crosses the network in the clear on a non-loopback HTTP endpoint | `cmd/msc/main.go:206`, `internal/mcpclient/client.go:234` | Low (user-configured remote endpoint) | Full vault read/write for the token's lifetime | Warn only; `--mcp-url` scheme validated, TLS 1.3 floor for HTTPS |
| 5 | The MITM CA private key is a machine-wide decryption key for everything the child sends | `internal/mitm/ca.go:140` | Low (requires local file read as another user, or root) | Decryption of any session routed through msc, and minting leaves trusted by anything that imported `msc ca` | `0600` key in a `0700` dir, permission re-check and warn on every load |
| 6 | Unbounded concurrent buffering on a loopback listener with no connection cap | `internal/proxy/proxy.go:54`, `:58`, `:264` | Low (local, requires many concurrent clients) | msc memory exhaustion, agent outage | Per-body size caps, 30s `ReadHeaderTimeout` against slowloris, timeouts on every leg |
| 7 | Cross-project memory bleed through the default vault name | `internal/config/config.go:201` | Low (two projects with the same directory base name, or one shared checkout) | Content from one codebase injected into an agent working on another | None; `--vault` / `MSC_VAULT` override |

## 1. Attack surface inventory

Entry points. Nothing in this repository listens on a
non-loopback address, and no third-party Go module is linked (`go.mod` has no
`require` block), so the dependency surface is the standard library plus
whatever the launched agent and MuninnDB themselves load. No entry point is
remote-facing; the only outbound sockets are to operator-named endpoints.

| Entry point | Address | File |
|-------------|---------|------|
| HTTP reverse proxy | `127.0.0.1:0` (random port) | `cmd/msc/main.go:362`, `internal/proxy/proxy.go:273`, `:344` |
| Session status endpoint (`GET`/`HEAD /__msc/health`) | same listener, unauthenticated | `internal/proxy/proxy.go:73`, `:379`, `serveStatus` at `:420` |
| HTTPS `CONNECT` proxy (`--mitm`) | same listener | `internal/proxy/proxy.go:184`, `internal/proxy/mitm.go:59` |
| Blind TCP tunnel to any `host:port` (non-intercepted CONNECT targets) | same listener | `internal/proxy/mitm.go:272` |
| WebSocket upgrade splice + decoder | reached only through the CONNECT tunnel | `internal/proxy/mitm.go:217`, `internal/proxy/wscapture.go:254` |
| SSE / ndjson response tap | upstream response bodies | `internal/proxy/stream.go:40` |
| CLI flags and passthrough agent argv | `msc <agent> [args...]` | `cmd/msc/flags.go:76`, `internal/agents/agents.go:520` |
| `msc` subcommands (`ca`, `status`, `list`, `help`, `version`, `completion`) | local process, stdout | `cmd/msc/main.go:119`, `cmd/msc/commands.go:319`, `cmdCA` at `cmd/msc/commands.go:60` |
| Environment variables | `MUNINN_MCP_URL`, `MUNINN_TOKEN`, `MSC_VAULT`, `MSC_UPSTREAM_<AGENT>`, `GEMINI_API_KEY` (upstream-selector), `MSC_WS_DEBUG`, `OPENAI_API_KEY`, `SSL_CERT_FILE`, `SHELL` | `internal/config/config.go:59`, `:171`, `:205`, `internal/agents/agents.go:215`, `:226`, `internal/proxy/wscapture.go:26`, `cmd/msc/main.go:328`, `:150`, `internal/agents/agents.go:428` |
| Config files read from disk | `~/.muninn/mcp.token`, `~/.config/muninn-sidecar/mitm/{ca-key.pem,ca-cert.pem,ca-bundle.pem}` | `internal/config/config.go:29`, `internal/mitm/ca.go:80`, `internal/agents/agents.go:353` |
| Outbound JSON-RPC client to MuninnDB | `MUNINN_MCP_URL`, default `http://127.0.0.1:8750/mcp` | `internal/config/config.go:20`, `internal/mcpclient/client.go:67` |
| Outbound HTTPS to the LLM provider | resolved upstream, TLS 1.3 floor | `internal/proxy/proxy.go:209` |
| Optional grounding judge (HTTP URL or local CLI) | `--ground-url`, `--ground-cmd` | `internal/grounding/grounding.go:307`, `:269` |
| Launched child process | agent binary resolved via `PATH` | `internal/agents/agents.go:520` (MITM launch at `:486`) |
| Scheduled jobs | none | — |

Eval/bench/qa surface. Same trust posture as `msc`, different reach: each
reads a dataset file the operator points it at, and each can send that content
off the machine.

| Entry point | What it takes | File |
|-------------|---------------|------|
| `msc-eval` flags and its MuninnDB client | CLI argv; `-token` or `MUNINN_TOKEN` | `cmd/msc-eval/main.go:81` |
| `msc-qa` model endpoint (`-model-url`, `-model-key`) | corpus questions plus recalled passages, with a bearer key | `cmd/msc-qa/main.go:57`, `:58`, `cmd/msc-qa/models.go:33` |
| `msc-qa` grounding endpoint (`-ground-url`, `-ground-key`) | the same text, through `internal/grounding` | `cmd/msc-qa/main.go:68` |
| `msc-qa` dataset files (`-dataset`, `-squad-file`) | arbitrary local JSON, read whole into memory | `cmd/msc-qa/dataset.go:19`, `:67` |
| `msc-bench` corpus files (`-squad-file`, HotpotQA) | arbitrary local JSON | `cmd/msc-bench/squad.go:211`, `cmd/msc-bench/hotpot.go:21` |
| `msc-bench` rewrite endpoint (`-rewrite-url`, `-rewrite-key`) and judge (`-ground-url`, `-ground-key`) | the probe query, plus `OPENAI_API_KEY` if set | `cmd/msc-bench/main.go:66`, `:72` |
| `msc-bench` / `msc-qa` `-ground-cmd` / `-rewrite-cmd` / `-model-cmd` | operator-named CLIs, whitespace-split or CSV, exec'd with the prompt on stdin | `internal/grounding/grounding.go:269`, `cmd/msc-qa/models.go:156`, `cmd/msc-bench/rewrite.go:190` |
| `msc-bench -dump-qa` | writes a JSON file the operator names | `cmd/msc-bench/main.go:59`, `:186` |

Deployment surface: there is no container image, service unit, or compose file
in the tree, so msc has no admin port, no container-exposed listener, and no
default service to account for. The only deployment-adjacent surface is
`.github/workflows/ci.yml` (test, lint, build, repro, fuzz, and vuln jobs), which
runs untrusted build input, the repository's own code, with no secrets beyond
the default `GITHUB_TOKEN` and read-only `contents: permission`
(`.github/workflows/ci.yml:12`). Developer tooling that reaches the network:
`scripts/fetch_hf_datasets.py` downloads third-party corpora over
`urllib.request` for `msc-bench` / `msc-qa` to consume; that content is
third-party text and reaches a judge model through the fenced prompt described
in [ARCHITECTURE.md](../ARCHITECTURE.md#untrusted-recalled-content).

`msc ca` is a surface in its own right even though it prints only public
material: it creates the CA on disk if absent (`cmd/msc/commands.go:60` →
`internal/mitm/ca.go:80`) and, with `-j`, writes the CA **certificate** PEM to
stdout. The key never leaves `ca-key.pem`. `msc status` is an MCP client call
like any other, and `completion` only prints a static script.

Treated as untrusted, correctly: the request body and response body from the
agent and the provider (`internal/proxy/proxy.go:546`, `:697`), the MuninnDB
JSON-RPC response (`internal/mcpclient/client.go:251`), the WebSocket frames
(`internal/proxy/wsframe.go`), the usage counters taken from an upstream body
(`internal/proxy/proxy.go:870`), and every recalled memory string
(`internal/inject/format.go:185`). The eval CLIs treat their dataset files the
same way in kind but not in size: `os.ReadFile` with no cap
(`cmd/msc-qa/dataset.go:19`, `cmd/msc-bench/hotpot.go:21`), so a hostile
multi-gigabyte file is an operator mistake, not a remote input. Model and
judge responses from a non-loopback endpoint are parsed as untrusted data and
capped (`cmd/msc-qa/models.go:52`, `internal/grounding/grounding.go:250`), but
their text lands in a scoring report on the operator's terminal, not in an
agent's system prompt, so the injection consequence of section 2 boundary 4
does not apply to them.

## 2. Trust boundaries

1. **Local process → proxy listener.** Any process on the machine can reach the
   loopback port. There is no authentication, no peer-credential check, and no
   token check; the proxy accepts any request, and `/__msc/health` answers any
   caller with the agent name, redacted upstream, uptime, and the full counter
   snapshot (`internal/proxy/proxy.go:420`). This is the widest boundary in the
   system and the one the docs do not describe.
2. **Agent → msc → LLM provider.** The agent's API key rides in an `Authorization`
   header through msc in cleartext on loopback, and msc deliberately adds no
   `X-Forwarded-For` so the upstream cannot see the interception
   (`internal/proxy/proxy.go:242`).
3. **msc → MuninnDB.** Bearer token in an `Authorization` header
   (`internal/mcpclient/client.go:124`, `:234`). Plaintext unless the endpoint is
   `https` or loopback; a warning is emitted, not an error
   (`cmd/msc/main.go:206`).
4. **MuninnDB vault → agent prompt.** The sharpest logical boundary. Vault
   content written by any session or any client is read back and spliced into
   the system prompt as a `<retrieved-context>` block
   (`internal/inject/inject.go:491`, `internal/inject/format.go:160`). Selection
   is relevance-based, not provenance-based. Recalled text is untrusted: block
   markers in it are neutralized so a memory cannot close its own block
   (`internal/inject/format.go:185`), and each block opens with a
   data-not-instructions notice (`internal/apiformat/apiformat.go:40`,
   written at `internal/inject/format.go:170`).
5. **msc → child agent process.** The child inherits the full parent environment
   (`internal/agents/agents.go:315`) and gains, under `--mitm`, trust of msc's
   CA plus proxy env vars that redirect its whole HTTPS traffic
   (`internal/agents/agents.go:486`).
6. **Local disk → msc.** The MITM CA key and the MuninnDB token file are read
   from fixed paths in the user's home.
7. **CLI flag → subprocess.** `--ground-cmd` is split on whitespace and executed
   with the query and recalled passages on stdin, never argv
   (`internal/grounding/grounding.go:269`). `msc-qa -model-cmd` takes the same
   shape as a comma-separated list (`cmd/msc-qa/main.go:60`).
8. **Dataset file → eval tool → model endpoint.** A path from the operator's
   command line selects a corpus to parse (`cmd/msc-qa/dataset.go:94`), and a
   second operator flag names where the questions and recalled passages go
   (`cmd/msc-qa/main.go:57`, `cmd/msc-bench/main.go:72`). Two independently
   chosen names sit on either side of one network hop, and the second defaults
   to nothing, so the exposure is a configuration choice rather than a
   default. The same pattern exists for the grounding judge in `msc`, where
   `OPENAI_API_KEY` may follow it (`cmd/msc/main.go:328`).

Privilege transitions: the agent's API key, the MuninnDB bearer token, and the
CA signing key are the three credentials msc holds in the request path. None is
ever persisted by msc except the CA key, which it generates. `OPENAI_API_KEY` is
a fourth, and it is the one credential msc may send to an operator-named
third-party host: msc warns for a non-TLS endpoint and for any host that is not
`api.openai.com` (`cmd/msc/main.go:328`, `internal/config/config.go:108`), but
proceeds.

## 3. Assets

- **Conversation content**: prompts, code, file contents, tool output. Held in
  MuninnDB indefinitely.
- **LLM provider API keys**: transit only, in the request header.
- **MuninnDB bearer token**: grants full read/write on the memory vault
  (`internal/config/config.go:167`).
- **MITM CA private key**: can mint a certificate for any host
  (`internal/mitm/ca.go:237`). Ten-year validity (`internal/mitm/ca.go:41`),
  renewed only within 30 days of expiry.
- **`OPENAI_API_KEY`**: sent to whichever grounding endpoint `--ground-url`
  names.
- **Session operational state**: the counters and identifiers on
  `/__msc/health` tell a local reader which agent is running, against which
  upstream, and whether captures are succeeding.
- **Availability of the agent session**: the proxy sits in the hot path, so
  msc crashing or stalling breaks the agent.
- **Reputation of the user's code**: captured content leaves the machine to the
  provider and, with `--no-redact`, is stored unscrubbed.

## 4. Threats per boundary

**Local process → proxy (STRIDE: spoofing, information disclosure, DoS, EoP).**
Spoofing is trivial: the proxy cannot tell the agent from any other local
client. The status endpoint is the cheapest read available, returning agent,
upstream, uptime, and capture counters to any caller
(`internal/proxy/proxy.go:420`). Under `--mitm` with the default intercept-all
scope (`internal/proxy/proxy.go:184`), any local client presenting any SNI
receives a minted leaf and its decrypted traffic is captured and stored, so a
local process can deliberately write into the user's vault, including content
shaped to steer a later agent session (threat 2 above). Non-intercepted hosts are
blind-tunneled to any `host:port` the client names
(`internal/proxy/mitm.go:272`), which makes msc a general local TCP relay with a
30s dial timeout and no destination policy. DoS: no connection cap, no
per-client quota; each in-flight request can hold up to 50 MiB of buffered
body on the request and response legs (`internal/proxy/proxy.go:54`, `:58`).

**msc → MuninnDB (tampering, repudiation, information disclosure).** The
response body is size-capped at 10 MiB and JSON-RPC protocol-level errors are
checked rather than treated as success (`internal/mcpclient/client.go:251`,
`classifyResponse` at `:261`), so a hostile or broken server cannot
silently corrupt a write. What msc does not do is authenticate the response body
beyond "it was valid JSON": a compromised MuninnDB can inject arbitrary text
that is later injected into an agent's system prompt. Over HTTP to a remote
host, the bearer token and the memory content are both on the wire in the clear.

**Vault → prompt (tampering, elevation of privilege).** Recalled memories are
placed at system-priority, above the user's own instructions. Controls: a
cosine confidence gate, dropping memories MuninnDB flags `archived`,
`cancelled`, or `untrusted`, near-duplicate removal, and a token budget
(`--inject-budget`, default 2048). There is no escaping, sandboxing, or
provenance check on the text itself, and no per-writer namespace. Redaction runs
on the injected text always (`internal/inject/format.go:186`) but redacts
credentials, not instructions.

**Agent → provider (information disclosure, repudiation).** msc reads and
rewrites the request body on every captured path, so it is a privileged
interceptor by construction. It is transparent on the wire (no added headers)
but not invisible: `--debug` logs paths and upstream URLs with the query string,
userinfo, and fragment replaced (`internal/proxy/proxy.go:965`), and
`MSC_WS_DEBUG` logs WebSocket message types and sizes, never content
(`internal/proxy/wscapture.go:275`).

**Disk → msc (spoofing, information disclosure).** Both the token file and the
CA key are read from fixed paths; msc warns but continues when their
permissions are loose (`internal/config/config.go:190`,
`internal/mitm/ca.go:101`). A local user who can replace `ca-key.pem` with a
readable file, or who points `MUNINN_MCP_URL` at a host they control, captures
the token and the memory traffic.

**Flag → subprocess (elevation of privilege).** `--ground-cmd` executes an
arbitrary command from the operator's own command line, bounded by a timeout,
with the query and passages on stdin. The blast radius is the user's own shell
authority, so this is a documented capability rather than a boundary crossing
by an attacker. `msc-qa -model-cmd` is the same capability with a wider fan-out:
it runs a comma-separated list of reader CLIs and takes the last non-empty
stdout line from each as an answer (`cmd/msc-qa/main.go:275`).

**Dataset file → model endpoint (information disclosure, spoofing).** The
corpus is whatever the operator's path resolves to, including a
`scripts/fetch_hf_datasets.py` download, and the recipient is a host named on a
second flag. The endpoint is unauthenticated from the server's side: nothing
verifies that the model host is the one the operator had in mind beyond a
warning for a non-OpenAI, non-loopback host
(`cmd/msc-qa/main.go:129`). Secrets in the question are redacted and the
question is fenced before it is sent (`cmd/msc-qa/models.go:194`), which
lowers the content exposure but does nothing for the endpoint's identity.
Spoofing the DNS or the endpoint captures the
corpus, and a response is parsed as a model answer and folded into a score, so
a hostile endpoint also chooses the numbers the run reports. The cap on the
response (`cmd/msc-qa/models.go:52`) bounds memory, not trust.

## 5. Mitigations, mapped

| Control | Covers | File |
|---------|--------|------|
| Loopback bind on a random port | remote network access | `cmd/msc/main.go:362` |
| Reserved `/__msc/` path, served before the capture pipeline, never forwarded or stored | operator status without leaking into the vault | `internal/proxy/proxy.go:73`, `:379` |
| No `X-Forwarded-For`; no User-Agent change (`Rewrite` instead of `Director`) | upstream cannot fingerprint the interception | `internal/proxy/proxy.go:242` |
| Request/response body caps (50 MiB), stream line cap (1 MiB), text accumulation cap (16 KiB), gzip decompression cap (50 MiB), MCP response cap (10 MiB) | memory exhaustion, gzip bomb | `internal/proxy/proxy.go:41`, `:44`, `:49`, `:54`, `:58`, `internal/mcpclient/client.go:26` |
| `ReadHeaderTimeout` 30s on the listener and on each tunnel | slowloris | `internal/proxy/proxy.go:264`, `internal/proxy/mitm.go:184` |
| Bounded CONNECT handshake (30s), upgrade reply (30s), and tunnel dial (30s) | a hijacked goroutine and its sockets pinned forever | `internal/proxy/mitm.go:33`, `:40`, `:25` |
| Leaf certificate cache capped at 1024 entries; host name length capped at 253 | unbounded CA memory, adversarial SNI | `internal/mitm/ca.go:60`, `:65` |
| Usage counters from an untrusted body range-checked to 0 on NaN, negative, or out-of-range values | a hostile body turning a float→int conversion into a garbage session total | `internal/proxy/proxy.go:870` |
| TLS 1.3 floor to the real upstream, normal certificate verification; TLS 1.2 floor with normal verification on the MITM forward leg | msc never forges trust toward the provider, and never trusts a bad upstream | `internal/proxy/proxy.go:209`, `:334` |
| TLS 1.3 floor for HTTPS MuninnDB; `--mcp-url` scheme and host validated at startup | downgrade, undialable config | `internal/mcpclient/client.go:74`, `internal/config/config.go:121`, `cmd/msc/main.go:194` |
| CA key `0600` in a `0700` directory, `chmod` forced on rewrite, permission re-check on every load | CA theft by another local user | `internal/mitm/ca.go:81`, `:140`, `:147`, `:101` |
| Combined system-roots + CA bundle for the vars that replace the trust store | breaking TLS to blind-tunneled hosts | `internal/agents/agents.go:453` |
| Secret and PII redaction before storage, on by default, `--no-redact` warns loudly | credential persistence | `internal/redact/redact.go:63`, `internal/store/muninn.go:207`, `cmd/msc/main.go:232` |
| Redaction always on the inject side, even with `--no-redact` | re-transmitting a secret stored by another client | `internal/inject/format.go:185` |
| Anti-recursion filters strip injected markers, `muninn_*` tool traffic, system reminders | a memory of a memory | `internal/proxy/filter.go:43`, `:86` |
| Block markers in recalled text neutralized; every block opens with a data-not-instructions notice | a memory forging the end of its own block | `internal/inject/format.go:170`, `:186` |
| Grounding prompt fenced per passage, newlines flattened, tag fence escaped | a passage forging a passage boundary or a verdict | `internal/grounding/grounding.go:77` |
| Judge prompt on stdin, never argv; judge stdout into a fixed-size tail buffer | prompt text exposed via `/proc`; unbounded judge output | `internal/grounding/grounding.go:269`, `internal/tailbuf/tailbuf.go` |
| 65 fuzz targets on the untrusted-input surfaces: `proxy` (17), `inject` (11), `msc-bench` (6), `apiformat` (6), `msc-qa` (5), `msc` (3), `mitm` (3), `agents` (2), `grounding` (2), `mcpclient` (2), `redact` (2), `store` (2), `querysplit` (1) | parser crashes on hostile input | `internal/proxy/fuzz_test.go`, `internal/inject/fuzz_test.go`, `internal/apiformat/fuzz_test.go`, `internal/mcpclient/fuzz_test.go`, `internal/store/fuzz_test.go` (method in [testing.md](testing.md)) |
| `govulncheck ./...` in CI; no third-party modules | known vulnerabilities | `.github/workflows/ci.yml:230`, `go.mod` |
| Bounded async queue (256) and 8s drain | capture loss under store outage, unbounded growth | `internal/store/muninn.go:201`, `:133` |
| Sanitized JSON error responses; query, userinfo, and fragment redacted in logs | stack traces and API keys in logs and client bodies | `internal/proxy/proxy.go:769`, `:965` |
| Warnings for plaintext HTTP to a non-loopback MuninnDB, and for `OPENAI_API_KEY` to a non-OpenAI or non-TLS grounding endpoint | silent credential exposure to an operator-misconfigured host | `cmd/msc/main.go:206`, `:328` |
| Warning naming the process list and shell history when a secret is passed as a flag, in all four binaries | token or API key exposed through `ps` and the shell's history | `internal/config/config.go:93`, `cmd/msc/main.go:204`, `cmd/msc-eval/main.go:81`, `cmd/msc-bench/main.go:152`, `cmd/msc-qa/main.go:142` |
| Eval endpoint URLs scheme- and host-validated at startup; `OPENAI_API_KEY` fallback warns for a non-OpenAI, non-loopback `-model-url` | an undialable or hostile endpoint reached only after the corpus is in flight | `cmd/msc-qa/main.go:107`, `:129`, `config.ValidateURL` at `internal/config/config.go:121` |
| Model and judge responses capped (4 MiB each), judge stdout into a fixed tail buffer | a hostile or broken model endpoint exhausting memory or unbounded output | `cmd/msc-qa/models.go:40`, `internal/grounding/grounding.go:39`, `internal/tailbuf/tailbuf.go` |
| Dataset loaders reject an empty or wrong-shaped file instead of reporting a run over zero questions | a silent all-zeros evaluation read as a passing one | `cmd/msc-qa/main.go:153` |
| Secret redaction and prompt fencing on the eval model and rewrite paths too, not just on `msc` | a credential inside a corpus question reaching an operator-named endpoint | `cmd/msc-qa/models.go:194`, `cmd/msc-bench/rewrite.go:52` |

No mitigation, by threat: the local listener accepts unauthenticated clients
and answers `/__msc/health` to any of them (risk 1); vault content is not
treated as untrusted instruction text (risk 2); redaction is pattern-based and
cannot be complete (risk 3); the plaintext-HTTP token path is a warning, not a
refusal (risk 4); the default vault name is directory-derived with no project
scoping (risk 7). On the eval side, nothing verifies that a model or judge
endpoint is the host the operator intended, and a response is scored without
being attributed, so abuse case 9 rests on a warning alone.

Single points of failure worth naming: the redaction pattern set is the only
control between captured content and the store, and the relevance gate is the
only control between the store and the agent's system prompt. Neither has a
second layer.

## 6. Abuse cases

These are scenarios with the enabling code path named. None was attempted.

1. **Local scrape into the vault.** A process started by the user, or by a
   malicious dev dependency, connects to the proxy port, sends a request on a
   capture path (`internal/proxy/proxy.go:537`), and its body is stored in the
   user's vault (`internal/proxy/proxy.go:743`). Under `--mitm` the same process
   can additionally ask for any host's tunnel and have its decrypted body
   captured (`internal/proxy/mitm.go:59`).
2. **Session reconnaissance.** The same process issues
   `GET /__msc/health` and learns which agent is running, which upstream it is
   pointed at, how long the session has been up, and whether captures are
   succeeding or erroring (`internal/proxy/proxy.go:420`), enough to tell
   whether a prompt it plants is being stored.
3. **Poisoned memory.** A planted memory that is semantically close to a future
   query passes the cosine gate and is injected at system priority
   (`internal/inject/inject.go:491`). Nothing records which session or process
   wrote it; the vault is keyed by name only
   (`internal/config/config.go:201`).
4. **Cross-project bleed.** Two checkouts whose directories share a base name
   (for example two `src` trees) resolve to the same vault, so decisions made
   in one are injected into the other.
5. **Resource exhaustion.** Concurrent large bodies on the loopback listener
   consume up to 100 MiB of buffers per in-flight exchange with no connection
   cap (`internal/proxy/proxy.go:54`, `:58`).
6. **Relay abuse.** With `--mitm`, a local client gets a TCP tunnel to any
   reachable `host:port` (`internal/proxy/mitm.go:272`), usable to reach
   services bound to loopback that the client could otherwise reach only as the
   same user.
7. **Over-collection.** `--mitm` without `--mitm-host` decrypts every host the
   agent connects to (`internal/proxy/proxy.go:184`), including hosts unrelated
   to the LLM API. `--mitm-host` narrows this and blind-tunnels the rest.
8. **Judge redirection.** A user who leaves `--ground-url` pointing at a
   third-party endpoint sends the query and the recalled passages of every
   turn there; msc warns once and continues (`cmd/msc/main.go:328`).
9. **Corpus exfiltration through an eval flag.** An `msc-qa` or `msc-bench`
   invocation against a downloaded corpus with `-model-url`,
   `-ground-url`, or `-rewrite-url` pointed at a host the run does not own
   sends questions and recalled memory text there, and the responses decide
   the scores the run prints (`cmd/msc-qa/models.go:33`,
   `cmd/msc-bench/main.go:72`). An operator who copied a command line from
   [docs/experiments.md](experiments.md) has this one flag away.
10. **Secret in the process list.** `-token`, `-model-key`, `-ground-key`,
   and `-rewrite-key` take secrets on argv, where `ps` and the shell history
   expose them. All four binaries warn, and none refuses
   (`internal/config/config.go:93`).
11. **Retaliatory reader CLIs.** `-model-cmd` runs an operator-named list of
   CLIs, comma-separated, once per question (`cmd/msc-qa/main.go:275`), so a
   long `-n` multiplies process spawns; the timeout bounds each call, not the
   count.

## 7. SECURITY.md accuracy

Checked on 2026-09-27. The claims in [SECURITY.md](../SECURITY.md) about the
CA key mode, trust scoping to the child, redaction being best-effort and
on by default, the token file location, the loopback listener, the
standard-library-only build, and `govulncheck` in CI all match the code at the
references given in the mitigation table above. Two facts were undocumented and
have been added there: the listener is unauthenticated (now including the
`/__msc/health` endpoint it serves), and vault content is injected into the
agent's system prompt. Two further surfaces were added on this pass: the
grounding judge as a third-party recipient of the query and the recalled
passages, and `msc ca` as the documented way to widen the CA's trust beyond the
launched child.

The eval CLIs were the one scope claim this document did not back with an
inventory. They are now enumerated in section 1, carry a boundary
(section 2, #8), a threat (section 4), four mitigation rows (section 5), and
three abuse cases (section 6), and [SECURITY.md](../SECURITY.md) now names
their endpoint and argv-secret exposure, which it previously did not mention at
all. The claims added there were checked against `cmd/msc-qa/models.go:194`,
`cmd/msc-qa/main.go:129`, and `internal/config/config.go:93`.

## 8. Response readiness

Recorded as-is; no owner or process is invented here.

- **Audit trail.** msc logs to stderr through `slog`, WARN by default, with
  `--log-json` for aggregation. Upstream 4xx/5xx responses are logged with
  status, method, path, agent, and duration
  (`internal/proxy/proxy.go:650`). There is no log of what was captured or
  stored: no exchange ids, no vault writes, no record of an injected block, and
  no record that a memory was selected or dropped. After an incident, the only
  available evidence is the MuninnDB content itself.
- **No record of local listener clients.** Requests are logged with method and
  path only, and nothing distinguishes the launched agent from any other local
  client, so misuse of the loopback port (risk 1) is not detectable after the
  fact. `/__msc/health` serves the aggregate of the same counters, so it is
  not an audit record either.
- **Disclosure path.** [SECURITY.md](../SECURITY.md) points at GitHub private
  vulnerability reporting. The document states the reporting channel but not
  who triages it, what the response target is, or what a reporter should
  expect after acknowledgement.
