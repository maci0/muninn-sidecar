# Changelog

All notable changes to `msc` (muninn sidecar) are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com); versions follow SemVer.

## [Unreleased]

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

[0.1.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.1.0
