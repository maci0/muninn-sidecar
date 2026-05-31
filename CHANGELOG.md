# Changelog

All notable changes to `msc` (muninn sidecar) are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com); versions follow SemVer.

## [Unreleased]

### Removed

- **Gated `antigravity` agent.** The experimental `antigravity` registry entry
  (hidden behind `MSC_EXPERIMENTAL_ANTIGRAVITY=1`) and that env gate are gone.
  Google's Antigravity CLI is supported via the `agy` agent only.

## [0.3.0] — 2026-05-31

Headline: **codex ChatGPT-mode is now captured** — msc decodes codex's
permessage-deflate WebSocket under `--mitm` — alongside a full secret-redaction
system, CI hardening (vulnerability scanning, green pipeline), and MITM
scoping/diagnostics.

### Added

- **codex ChatGPT-mode capture (WebSocket).** codex in ChatGPT-subscription mode
  streams the OpenAI Responses API over a permessage-deflate WebSocket (ignoring
  `OPENAI_BASE_URL`). Under `--mitm`, msc now decodes that stream — RFC 6455
  framing + RFC 7692 context-takeover inflation — accumulates the
  `response.output_text` deltas, pairs them with the `response.create` request,
  and stores the turn through the normal pipeline (extraction, secret redaction,
  dedup). Decoding runs on a best-effort copy that abandons under backpressure,
  so it never blocks or alters the agent's connection. Verified live end-to-end.

### Fixed

- **Explicit JSON content negotiation for MCP calls** — requests now send
  `Accept: application/json` so an MCP-over-HTTP server capable of both JSON and
  SSE returns JSON (what the one-shot JSON-RPC client parses) rather than possibly
  defaulting to a `text/event-stream` reply.
- **Bounded shutdown when MuninnDB is unreachable** — `Drain` now arms a deadline
  that cancels in-flight flush retries, so Ctrl-C with a queued backlog against an
  unreachable MuninnDB exits within ~8s instead of retrying ~6s per queued batch
  (which could stack to minutes). Flush calls are now context-aware (interruptible
  backoff); a single in-flight batch still gets its full retry budget for
  transient blips.
- **SSE capture without the optional space** — the streaming parser now accepts
  `data:{...}` (no space after the colon), per the SSE spec's optional leading
  space. The big-3 APIs send `data: `, but OpenAI-compatible proxies and local
  servers may omit it; previously those deltas were silently skipped.
- **MITM upgrade-splice dial timeout** — the WebSocket/upgrade splice now dials
  the backend with a 30s timeout (mirroring the blind-tunnel), so a black-hole
  target can't hang the goroutine and its hijacked connection indefinitely.
- **MITM WebSocket/`101` upgrades** — intercepted protocol-upgrade requests (e.g.
  codex ChatGPT-mode streams over a WebSocket) are detected and spliced raw to
  the backend over TLS instead of erroring in the capturing reverse-proxy.
  Verified live: codex ChatGPT-mode now runs cleanly through `--mitm`.

### Added

- **Secret redaction before storage** — captured exchanges are scanned for
  well-known credential formats (OpenAI/Anthropic `sk-` keys, AWS access keys,
  GitHub tokens incl. fine-grained PATs, Google API keys, Slack tokens, Stripe
  secret/restricted keys, npm tokens, JWTs, `Bearer` and Basic auth headers, PEM
  private-key blocks) and replaced with `[REDACTED]` before being written to
  MuninnDB, so secrets pasted into an agent don't persist and resurface via
  recall. Patterns are conservative (prefix/structure-anchored) to avoid
  corrupting prose. Also catches sensitive `key=value` / `key: value` assignments
  (`API_KEY=…`, `DB_PASSWORD: …`, `client_secret=…`, incl. identifier-prefixed env
  vars) — the common case of a pasted `.env` file or shell export — redacting the
  value while keeping the key for context. Applied on **both** sides: before
  storing a captured exchange (disable with `--no-redact` for full-fidelity local
  capture in trusted environments), and — always, as defense in depth — to
  recalled memory content before it is injected into an outgoing request, so a
  secret stored by another client or before redaction existed isn't re-transmitted
  to the provider in a session where it wasn't otherwise present. (Redaction logic
  lives in the shared `internal/redact` package.)
- **`msc status` vault stats** — when MuninnDB is reachable, `status` now reports
  the vault's memory count and health (via the `muninn_status` tool), and flags an
  empty vault — directly answering "why is nothing being injected?". Best-effort:
  omitted (not an error) on servers without the tool. `--json` gains `memories` /
  `vault_health`.
- **`msc ca` command** — prints the TLS-MITM CA certificate path + SHA-256
  fingerprint (creating the CA if needed); `--json` includes the PEM. Lets users
  trust msc's CA in tools it doesn't launch itself (browsers, system store,
  custom HTTPS clients) for the transparent-HTTPS-proxy use case.
- **Upgraded-stream visibility** — the session summary reports spliced WebSocket/
  upgrade streams (`mitm: N WebSocket/upgrade stream(s) spliced`). codex's stream
  is decoded and captured (see Added); other WebSocket protocols pass through
  without capture.

## [0.2.0] — 2026-05-31

Headline: **opt-in TLS-MITM interception** (`--mitm`) — capture and inject for
agents that ignore a base-URL override, by acting as a transparent HTTPS proxy
with a locally-trusted CA.

### Added

- **TLS-MITM interception (`--mitm`, opt-in)** — intercept agents that don't
  honor a base-URL env override (codex ChatGPT-mode, grok session auth, agy) and
  use msc as a transparent HTTPS proxy. A local certificate authority
  (`internal/mitm`) auto-generates/persists a CA (0600 key, local-only, under the
  user config dir) and mints cached per-host leaf certs signed by it. With
  `--mitm`, msc accepts `CONNECT` tunnels, terminates TLS with a minted leaf,
  runs the decrypted request through the same recall/inject + capture pipeline as
  the plain path, and re-originates TLS to the real host. The child is pointed at
  msc via `HTTP(S)_PROXY`/`ALL_PROXY` (upper and lower case) and told to trust the
  CA via `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `REQUESTS_CA_BUNDLE` /
  `CURL_CA_BUNDLE` / `DENO_CERT`, plus `NODE_USE_ENV_PROXY=1`. Off by default; the
  CA private key never leaves the machine and trust is scoped to the launched
  child only, never the system trust store. Interception verified per-runtime
  (Node/undici, Rust/reqwest, Bun, Deno, Python, Go) — notably, Node's global
  `fetch` ignores `HTTPS_PROXY` without `NODE_USE_ENV_PROXY=1`, so msc sets it.
- **`proxy.SetMITMRoots`** — override the root CAs used to verify real upstreams
  on the MITM forward leg (private/corporate upstream CA, or tests).
- **`--mitm-host` scoping** — by default `--mitm` intercepts every CONNECT host;
  `--mitm-host HOST` (repeatable/comma-separated, implies `--mitm`) limits TLS
  termination to the upstream + listed hosts and blind-tunnels everything else
  untouched, so package registries and cert-pinned services aren't decrypted.

### Fixed

- **MITM WebSocket/`101` upgrades** — intercepted protocol-upgrade requests
  (e.g. codex ChatGPT-mode streams over a WebSocket) are now detected and spliced
  raw to the backend over TLS instead of erroring in the capturing reverse-proxy
  (`internal error: 101 switching protocols response with non-writable body`).
  Verified live: codex ChatGPT-mode now runs cleanly through `--mitm` (was
  erroring + retrying). The upgraded stream itself isn't parsed for capture yet,
  so codex's WebSocket-framed turns aren't stored — but the agent works.

### Changed

- **MITM CA/leaf hardening** — the on-disk CA is regenerated on load when within
  30 days of expiry (no more leaves outliving their issuer); the per-host leaf
  cache is bounded (`maxCacheEntries`, evicts when full) so a long-running
  transparent proxy can't grow it without bound; expired cached leaves are
  re-minted on demand; leaf validity shortened to 24h. Concurrency-stress tested.

## [0.1.0] — 2026-05-31

First tagged release. A transparent reverse proxy that gives any stateless AI
coding agent long-term memory by capturing conversations into [MuninnDB](https://github.com/scrypster/muninn)
and injecting relevant recalled context — with zero agent configuration.

### Added

- **Transparent proxy** — overrides the agent's API base-URL env var, forwards
  all traffic unchanged (no extra headers / User-Agent), captures matching
  endpoints, and streams SSE responses through in real time.
- **Auto-memorization** — captured request/response exchanges are cleaned
  (injected-context markers, MuninnDB tool calls, system-reminders, and noise
  stripped) and stored asynchronously via MCP, batched with dedup and a
  flush-on-exit drain (headless `-p`/`exec` runs save correctly).
- **Auto-injection** — per request: recall on the latest user turn → gate on an
  auto-calibrated cosine confidence → drop unfit memories (`archived` /
  `cancelled` / `untrusted`) → resolve staleness and contradictions (current fact
  supersedes stale/contradicted, via MuninnDB `annotate:true`) → drop
  near-duplicates → pack within the token budget. Reuses the session window on
  unchanged-query continuations; injects nothing when no memory is confident.
- **Self-calibrating gate** — `MinScore` self-tunes per vault to the
  noise/relevant valley (Otsu), so it adapts to low-cosine deployments instead of
  a fixed cutoff; the recall floor sits below the calibration floor so it never
  caps the gate.
- **Optional answer-grounding rerank** (`--ground-url` / `--ground-cmd`) — a
  listwise LLM precision check (one call/turn) for harm-prone vaults; off by
  default, fails open. Local model (fast, in-flight) or frontier CLI (offline).
- **Supported agents** — `claude`, `codex`, `opencode`, `aider`, `grok`,
  `reasonix`, `qwen` (flag-injected base URL), plus `agy` (launch-only) and gated
  `antigravity`. Captures Anthropic, OpenAI, and Gemini/Code-Assist API formats.
  Caveats documented for OAuth-direct modes (codex ChatGPT-subscription, grok
  API-key requirement, agy) that bypass env-based interception.
- **Observability** — `msc status`, session summary (injected/suppressed,
  recalled/reused, grounding drops, budget truncation), `--json` output, shell
  completions, `--dry-run`.
- **Evaluation tooling** — `msc-eval` (offline selection-quality + threshold
  sweep + cross-validated method study), `msc-bench` (real-MuninnDB retrieval +
  gate + auto-calibration validation, hard-negative probes, grounding/rewrite),
  `msc-qa` (downstream answer-quality across models + frontier CLIs).
  `scripts/fetch_hf_datasets.py` seeds 10+ HuggingFace dataset regimes.

### Validated

- Downstream usefulness across ~10 local models and 7+ task regimes: injection's
  value ≈ retrieval accuracy × the model's in-context ability, and a wrong
  injection never helps — so the sidecar both recalls accurately and gates.
- Every function has tests; 40 fuzz targets over all parsing/transform surfaces;
  race-clean; CI builds all binaries, runs `go vet`/staticcheck/race tests, and a
  short fuzz campaign on every push.

[0.3.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.3.0
[0.2.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.2.0
[0.1.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.1.0
