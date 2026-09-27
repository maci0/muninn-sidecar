# Changelog

All notable changes to `msc` (muninn sidecar) are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com); versions follow SemVer.

`msc` is pre-1.0, so a **minor bump may carry breaking CLI, output, and
configuration changes** (see `CONTRIBUTING.md`). Every such entry opens with
`**Breaking:**`, so a reader does not have to assume the minor bump is safe.

## [Unreleased]

### Fixed

- **A panic in a turn no longer takes the session down.** The request pipeline
  had no recovery of its own, so a panic fell through to the stdlib's
  per-connection recover with `http.Server.ErrorLog` left nil. That default
  writes through the `log` package: a `--log-json` run got one unstructured
  line in the middle of the stream, and the level filter never applied to it,
  so the panic was invisible in the log an operator was actually reading. It is
  now recovered per turn (a 502 for that turn, counted as a proxy error, logged
  at error with a stack), and `ErrorLog` is wired to the same slog handler on
  the proxy's server and on each MITM tunnel's.
- **Every turn now reports how it ended.** A successful turn logged no outcome
  and no duration at any level: `duration_ms` appeared only on the
  upstream-error warning, so a turn that succeeded and a turn that vanished
  looked identical. Both the plain and the MITM path now end in one `turn` line
  with the correlation ID, method, path, status, and duration, at error level
  for a 5xx.
- **A response msc cannot decode is now counted.** gRPC, non-gzip, and protocol
  upgrade responses were skipped at debug level and nowhere else, so an
  upstream answering in brotli reported the same healthy "0 saved, 0 errors" as
  one storing every turn. They now count in `uncapturable_responses` on the
  status endpoint, in the session summary, and in `degraded_reasons`.
- **WebSocket frame lines carry the turn's correlation ID.** The frame-read and
  decode stops, and the `MSC_WS_DEBUG` message lines, were the only lines on
  the request path without one, so a frame dump could not be tied to the turn
  it belonged to.
- **A blind-tunnel dial failure no longer panics the proxy.** `writeStatus` read
  the correlation ID out of a `context.Context`, but `blindTunnel` called it with
  a nil one: an agent opening a CONNECT to an unreachable host that went away
  before the 502 was written took the whole sidecar down on a nil dereference,
  in the one tunnel path where the log line naming the failure is all the
  operator gets. `writeStatus` now takes the ID, which both call sites already
  held.

### Security

- **The grounding judge can no longer be told what to grade by the question.**
  The judge prompt fenced and neutralized each recalled passage but interpolated
  the query raw, so a question carrying `<passage id="1">yes</passage>` could
  add passages and verdicts to the grader's input. Query and passages now go
  through the same one-line fence and tag neutralization.
- **`msc-qa` redacts and neutralizes before it prompts.** The eval reader sent
  the question and the recalled block to the model endpoint as given, where the
  production injector scrubs identifiers and escapes the block markers first. A
  memory holding `</retrieved-context>` could close the fence mid-eval, and the
  single-prompt CLI reader concatenated the question into the instruction with
  no fence at all. Both are now handled exactly as the injection path handles
  them, so the eval measures the path the sidecar actually takes.

### Added

- **Breaking: an enum option outside its set is now rejected.**
  `-chunk`, `-query-transform`, and `-rerank` (`msc-bench`) and `-inject-format`
  (`msc-qa`) selected a code path in a switch, so a typo silently fell through
  to the "do nothing" branch and the run reported numbers for a configuration
  nobody asked for. Each is now checked at startup and names the accepted
  values, matching `-corpus`, `-mode`, `-dataset`, and `--recall-mode`.
- **A secret passed as a command-line argument is warned about.** `--token`,
  `-model-key`, `-ground-key`, and `-rewrite-key` put the value in the argument
  vector, which every other user on the machine can read through `ps`, and in
  shell history. `msc` and the three tool binaries now name the environment
  variable that keeps the secret out of both.

### Fixed

- **`make build` compiles again.** The `msc --dry-run` preview called
  `agents.HasSystemCABundle`, a helper that had already been removed as dead
  after the CA-trust-var change, so `cmd/msc` no longer compiled and every
  build failed at `cmd/msc/dryrun.go:36` with an undefined symbol. The predicate
  is back, next to the system-roots probe it answers for, so the preview and
  the real launch cannot disagree about it, and a test pins it to the bundle
  `writeCombinedCABundle` actually writes.
- **The CI lint job can pass again.** The presence gate that runs before
  shellcheck, ruff and yamllint stripped *every* space out of the tool variable
  before testing it, so the pipx form CI invokes was looked up as the single
  word `pipxrunruff@0.16.4` and the job failed with "ruff is required" on every
  run. It now gates on the first word, which is the executable the command
  actually runs.
- **`scripts/check-release-notes.sh` is linted.** `make lint-non-go` passed
  shellcheck a single file by name, so the release gate's script was never
  checked. It now passes every tracked `*.sh`, which covers new scripts without
  a second edit.
- **The ruff and yamllint pins have one home.** They were stated in the
  Makefile and repeated as literals in `ci.yml`, so a bump in one left the
  other enforcing a different ruff release than `make lint` did. `make lint-ci`
  is now the invocation the workflow runs, pins included.
- **Breaking: a bad flag value now exits 2 in every binary.** The `flag`
  package already exits 2 for an unparseable command line, but the three tool
  binaries exited 1 for a value it parsed and then rejected (`msc-qa -n 0`,
  `msc-bench -ground-topk 0`, `msc-eval -min-score 5`), so a script could not
  tell a typo from a runtime failure. The rejections are typed and exit 2, as
  `msc` already did.
- **`--mitm-host` with no host in it is rejected.** `--mitm-host ""` (or a
  value of only commas and spaces) scoped nothing and left `--mitm` on, which
  TLS-intercepts every host: the opposite of what the flag asks for, and the
  one setting that widens interception to hosts the user never named. It now
  fails as a usage error like every other empty flag value.
- **Two grounding backends in one command line are named.** `--ground-cmd` and
  `--ground-url` together silently dropped `--ground-url`; the run now warns
  which backend it picked.
- **`--json` output is no longer HTML-escaped.** The proxy placeholder in
  `msc --json --dry-run` came out as `\\u003cport\\u003e`, so a grep or a `sed`
  over the machine-readable form missed it.
- **A failed flush no longer leaves a dedup mark that swallows the retry.** The
  store's dedup ring recorded a concept's hash when the exchange was formatted,
  up to two seconds before the flush that writes it. A flush that failed
  permanently (a 4xx, or a retry budget spent against an unreachable server) left
  a "already stored" mark for a memory that was never written, so the user
  re-asking the same question inside the ~16s window was deduped away and lost.
  A hash now enters the ring only once its flush succeeds; a repeat inside the
  undelivered batch is collapsed against a per-batch pending set instead. The
  ring window is also rolled before each flush rather than after, so a delivered
  memory occupies the full ~16s window instead of being cleared by the same tick
  that stored it.
- **A `go.mod` bump cannot leave CI on an older Go.** `ci.yml` installs the
  release line in its own `GO_VERSION` and `go.mod` names the floor the module
  needs, with nothing comparing them: raise the directive and the build fails
  late as a `note: module requires Go X.Y` under an unrelated error, lower it and
  the tree compiles silently against a toolchain nobody chose. `make
  go-version-check`, part of `make check`, fails with both versions named.
- **Reader output and judge scope are capped at startup.** `-max-tokens`
  (`msc-qa`) and `-ground-topk` (`msc-qa`, `msc-bench`) were the only things
  bounding a model call, and neither rejected a zero: providers that honor
  `max_tokens` error out on it, the rest ignore it and generate until they stop,
  and a zero `topk` does not mean "grade none" but "grade every recalled
  passage in one call". Both now fail loud at startup, matching the `--ground-topk`
  check `msc` already had.
- **A capped server error message no longer bypasses redaction.** The RPC
  error text was capped at 300 runes *before* the scrub, so a rejection long
  enough to hit the cap (exactly the kind that quotes the memory it refused)
  returned its first 300 runes unredacted, putting the secret at the head of
  every log line the error reached. It is scrubbed first and capped second.
- **A judge CLI's output capture starts on a character boundary.**
  `internal/clirun` carried its own tail buffer that kept the last 4 MiB of
  bytes with no UTF-8 check, so a child that overran the cap mid-character (a
  CJK or emoji verdict) handed every caller invalid UTF-8 to parse. It now uses
  the shared `internal/tailbuf`, whose buffer is rune-aware.
- **A response body quoted into an error is cut on a character boundary.** The
  200-byte summary of an untrusted response could land inside a multi-byte
  character and put a replacement character in the log line for the rest of it.
- **A sub-query that begins with digits keeps them.** The list-marker strip
  removed a bare run of leading digits, so a rewriter answering "1989" to a
  year question lost the sub-query entirely and "2004年の…" became "年の…". A
  marker is now a bullet, or an index with its separator, and nothing else.
- **An entity span ends with its own script's punctuation.** `querysplit`
  trimmed only `?.,` off a capitalized run, so a span closed by `。` was a
  different recall key from the same name closed by `?`.
- **`MSC_WS_DEBUG=0` no longer turns the WebSocket probe on.** The switch was
  read as "set means on", so a shell profile that exported the variable to
  disable it kept the probe logging. `0`, `false`, `off`, and `no` now turn it
  off, as does leaving it empty; any other value turns it on.
- **A memory server's rejection text no longer reaches the log unscrubbed.**
  `muninn_remember` and `muninn_remember_batch` failures are reported at error
  level with the concept and content deliberately omitted, but the error itself
  carried the server's own message, which quotes the memory it refused. The
  text is now scrubbed of direct identifiers and capped before it becomes an
  error value, so no call site can leak captured conversation into a log line, a
  stderr warning, or an `err` field by way of the transport.
- **The store queue is bounded by bytes, not only by slot count.** Its
  back-pressure was a 256-slot channel, but an exchange carries a captured
  request that repeats the whole conversation (up to 50 MiB a body) plus its
  response, so a queue a few slots short of full could be holding tens of GiB
  and take the process down before the depth said anything was wrong. Producers
  now reserve each exchange's body bytes against a 256 MiB budget as they
  enqueue, the worker returns them once the exchange is formatted, and a capture
  that would pass the budget is dropped exactly as one that finds a full queue
  is. `GET /__msc/health` reports `bytes_in_flight`, `bytes_capacity` and
  `bytes_saturated` next to the depth, so a queue dropping on its memory budget
  no longer reads as healthy.
- **`msc-bench -seed` is idempotent again.** Seeded memories went out without
  the content-addressed `dedup_key` every other write path carries, so a rerun
  of `-seed` stored a second copy of the whole corpus. The duplicates crowd
  recall's top-k and skew the retrieval numbers the tool exists to measure.
- **A CRLF results file no longer grows without bound.** The `msc-qa` report
  writer matched a run's manifest marker against the line with only its newline
  trimmed, so in a file with Windows line endings the marker was never found and
  every rerun appended another block instead of replacing its own. The marker is
  now matched with the carriage return trimmed too, and a replacement block is
  written with the line ending the file already uses, so a report stays CRLF end
  to end.
- **Non-ASCII text no longer breaks at a character boundary.** The `msc-eval`
  and `msc-qa` report tables clipped a scenario name or model label by byte, so
  a multi-byte character (CJK, emoji) was cut in half and the row ended in a
  replacement character; both count characters now. A capped text delta in the
  SSE and WebSocket paths trimmed bytes from the end until the string decoded,
  which discarded every character after a stray invalid byte in the delta rather
  than just the incomplete tail; only the tail is dropped now. The bounded
  capture buffer kept the last N bytes of a stream, which can start part-way
  through a character, so its contents could no longer decode; it now advances
  to the first whole character.
- **A URL with no `//` after the scheme no longer logs its credentials.** The
  log sanitizer redacted userinfo held in `url.Userinfo`, but a URL parsed in
  the opaque form (`https:user:pass@host/v1`, which a client can put in an
  absolute-form request target) keeps that userinfo as literal text inside
  `Opaque`, so the password reached the logs and the stored exchange verbatim.
  Both carriers are redacted now, and a fuzz target holds the contract.
- **A Unicode host is refused when minting a certificate.** SNI and CONNECT
  carry punycode, and a certificate SAN cannot hold a non-ASCII name, so
  `--mitm-host` or a CONNECT naming `münchen.de` failed deep inside x509 (or,
  when only interception was scoped, silently blind-tunneled the host). The
  error now names the punycode form to use, and a host with an explicit DNS
  root (`api.openai.com.`) mints the same leaf as one without.

### Changed

- **The CI linters are pinned like the other tools.** `staticcheck` and
  `govulncheck` were installed with `go install <module>@latest`, so each run
  resolved whatever the public proxy named that day: a release with new checks
  turned a green push red with nothing in the diff to explain it, and one with
  loosened checks turned it green just as silently. Both now carry a version in
  the Makefile, overridable the same way, and `make lint` and `make vuln` warn
  when the copy on `PATH` is not the pinned one.
- **The threat model inventories the eval CLIs.** The document claimed the
  `msc-eval`, `msc-bench`, and `msc-qa` scope was covered "only where they
  differ materially" without ever listing what that is. Their entry points,
  the dataset-file-to-model-endpoint boundary, the mitigations that apply
  (argument-secret warnings, endpoint validation, response caps, redaction
  before prompting) and three abuse cases are now written down with file
  references, and `SECURITY.md` names the model and judge endpoint and the
  flags that put a secret in the process list, which it did not mention.
- **CI and `make lint` run the same linter invocations.** The `Lint` job kept
  its own copy of the four non-Go linter commands while the Makefile kept a
  second one, so the two could check different rules without either change
  looking wrong. The commands now live in the `lint-non-go` target, and CI
  calls that target with the pinned ephemeral ruff and yamllint passed in as
  make variables.
- **A failing sidecar says so, and a log line names its turn.** Three
  observability gaps on the request path:
  - The `request_id` stopped at the end of the HTTP request. The store's
    background worker, which sees every exchange in a session through one
    goroutine long after the turn returned, logged "queue full" and "failed to
    flush" with no way back to the turn that lost its memory, and the injector's
    recall failures never carried one at all. The ID is now minted in
    `internal/reqid`, travels on the `CapturedExchange`, and appears on the
    store, inject, MITM, and WebSocket-tap lines. A failed flush names the turns
    whose memories were not written.
  - Requests arriving inside a MITM tunnel are built by the tunnel's own
    `http.Server` and so arrived with no ID; they mint one per request now, and
    the codex WebSocket tap keeps the upgrade's ID for the exchanges it decodes
    off that one connection.
  - `GET /__msc/health` answered `"status":"ok"` while captures were being
    dropped and memories were never written. The status code stays liveness
    (restarting does not help, and the sidecar is still the agent's only route
    to the API), and the body now carries `degraded` with `degraded_reasons`
    naming the failing stage, plus the store queue's live depth and
    saturation, the signal that predicts the next drop. An upstream 4xx/5xx
    does not count: the provider refused, the sidecar worked.
  - `requests` counts everything the agent sent, so `requests / uptime_s` is the
    request rate; `captured` counts only what reached the store, so a session
    that is answering but no longer capturing is now distinguishable from one the
    agent has stopped calling.
- **Captured bodies are decoded once.** A captured request carries the whole
  conversation and can reach tens of MiB, and the store worker decoded it four
  times over: once to filter injected context and tool traffic, twice more to
  pull the model name and token usage, and again to find the last user message.
  The request and response are now each parsed a single time, and the filtering
  step hands its parsed document to the callers that read those fields. Capture
  normalization is ~1.8x faster on a 1 MB request body.

### Added

- **One clock for the whole sidecar.** `internal/clock` holds the project's only
  time source: `Clock` (now, elapsed, `After`, `NewTicker`, `AfterFunc`),
  `SystemClock` for production, and `Fake`, a manually advanced clock. The
  capture path's `proxy.Clock` is now that clock, and the store worker
  (`store.NewWithClock`) and the injector (`inject.Config.Clock`) read it too, so
  the store's flush ticker, dedup-ring expiry, retry backoff, and drain deadline
  and the injector's intent-cache TTL advance on a script instead of wall time.
  A simulated session can be replayed step for step, and the store's retry test
  no longer spends ~6s of real backoff.
- **The session is observable.** `--debug` output and the end-of-session summary
  were the only view of a session, and neither said which turn a line belonged to
  or which dependency had failed. Four things ship together:
  - Every request mints a `request_id` (`req-1`, `req-2`, …) carried in the
    request context, never in a header, so the forwarded request stays
    byte-identical to what the agent sent. Every log line on the request path
    carries it, so one agent turn can be picked out of an interleaved session.
  - Upstream 4xx/5xx responses are logged at warn with status, method, path,
    agent, and `duration_ms`, and counted as upstream errors. Failures msc
    causes itself (dial/TLS/transport error, the agent getting a 502) are logged
    at error and counted separately as proxy errors, so the summary distinguishes
    "the provider refused" from "the sidecar could not reach it". A client that
    cancels mid-flight is debug-level noise and counts as neither.
  - Response time is sampled once per completed exchange (full body for a
    non-streaming response, last byte for a stream) and reported as mean and max
    in the summary.
  - The proxy answers `GET /__msc/health` on its own port
    (`proxy.StatusPath`) with the agent, upstream, uptime, and a
    `stats.Snapshot` as JSON. It is a liveness endpoint and does not probe
    MuninnDB, so a MuninnDB outage shows up as climbing `save_errors` rather than
    as a failing probe.
- **`--log-json` emits JSON logs.** Text on stderr by default; `--log-json`
  writes the same lines as JSON objects, so a log pipeline can key on `level`,
  `msg`, and `request_id` without parsing a message string. The default level is
  unchanged (WARN), so a healthy session stays quiet.
- **Phone numbers are redacted.** Phone numbers passed every existing pattern
  and so persisted in long-term memory and were re-injected on recall. E.164
  (contiguous and grouped) and NANP forms are now scrubbed, with NPA/NXX
  leading-digit and grouping constraints that keep version strings, ISO dates,
  and IPv4 addresses intact.
- **Per-command help.** `msc list --help` (and `status`, `ca`, `version`,
  `completion`, `help`) printed the global usage, which documents the agent
  wrapper and cannot say what a single command accepts. Each command now has
  its own usage, arguments, examples, and exit codes, reachable as
  `msc <command> --help` or `msc help <command>`; `msc help <agent>` prints
  that agent's base-URL override, default upstream, and launch line.

### Changed

- **A failing sidecar says so, and a log line names its turn.** Three
  observability gaps on the request path:
  - The `request_id` stopped at the end of the HTTP request. The store's
    background worker, which sees every exchange in a session through one
    goroutine long after the turn returned, logged "queue full" and "failed to
    flush" with no way back to the turn that lost its memory, and the injector's
    recall failures never carried one at all. The ID is now minted in
    `internal/reqid`, travels on the `CapturedExchange`, and appears on the
    store, inject, MITM, and WebSocket-tap lines. A failed flush names the turns
    whose memories were not written.
  - Requests arriving inside a MITM tunnel are built by the tunnel's own
    `http.Server` and so arrived with no ID; they mint one per request now, and
    the codex WebSocket tap keeps the upgrade's ID for the exchanges it decodes
    off that one connection.
  - `GET /__msc/health` answered `"status":"ok"` while captures were being
    dropped and memories were never written. The status code stays liveness
    (restarting does not help, and the sidecar is still the agent's only route
    to the API), and the body now carries `degraded` with `degraded_reasons`
    naming the failing stage, plus the store queue's live depth and
    saturation, the signal that predicts the next drop. An upstream 4xx/5xx
    does not count: the provider refused, the sidecar worked.
  - `requests` counts everything the agent sent, so `requests / uptime_s` is the
    request rate; `captured` counts only what reached the store, so a session
    that is answering but no longer capturing is now distinguishable from one the
    agent has stopped calling.
- **Captured bodies are decoded once.** A captured request carries the whole
  conversation and can reach tens of MiB, and the store worker decoded it four
  times over: once to filter injected context and tool traffic, twice more to
  pull the model name and token usage, and again to find the last user message.
  The request and response are now each parsed a single time, and the filtering
  step hands its parsed document to the callers that read those fields. Capture
  normalization is ~1.8x faster on a 1 MB request body.

- **Breaking: a typo in the MuninnDB URL fails at startup.** `--mcp-url` and
  `MUNINN_MCP_URL` are validated before anything else runs, so a missing scheme,
  a non-HTTP scheme, or a missing host names itself instead of surfacing later as
  a transport error from the health check or as silent capture loss. All four
  binaries (`msc`, `msc-eval`, `msc-bench`, `msc-qa`) now resolve the endpoint,
  token, and vault from one package, so the default, the env var names, and the
  token-file path have a single definition.
- **The grounding key is checked against the host, not just the scheme.**
  `OPENAI_API_KEY` was warned about only over plaintext HTTP. It is now also
  warned about when `--ground-url` points at a host that is not api.openai.com,
  since the key then leaves the machine in a form the user may not have intended.
- **Capture no longer cleans bodies on the request path.** Body normalization ran
  between receiving a response and forwarding it, so its cost was paid by the
  agent's turn. It now runs on the store worker, after the bytes are forwarded.
- **Redaction is gated, and 13x faster.** Almost every pattern starts with a
  character class or word boundary rather than a literal, so the regexp engine
  fell back to a full scan per pattern. Each rule now carries a necessary
  condition (a literal or case-folded substring, a numeric-looking run, a
  sensitive key stem) checked before the regex runs; every condition is required
  by its pattern, so which spans get redacted is unchanged. A 4 KiB turn went
  from ~2.3ms to ~0.17ms.
- **CI lints the Python, shell, and YAML sources.** A `Lint` job runs `ruff`
  (plus `ruff format --check` on `scripts/`), `shellcheck` on `test-live.sh`,
  and `yamllint` on the repository's YAML, each with a pinned version, so the
  non-Go sources are held to the same bar as the Go tree.
- **Dev loop matches CI.** `make lint` and `make vuln` no longer swallow a
  linter's real failure behind a "not installed, skipping" message; a missing
  tool now fails with the `go install` line to run. `make check` reproduces the
  CI test job locally (tidy, gofmt, vet, staticcheck, race test, build), `make
  help` lists every target, `make tools` installs the two CI linters, and
  `make test PKG=... RUN='^TestFoo$'` (plus `make test-fast`) narrows the
  edit-test loop to one package or test.
- **Eval methodology hardened.** `msc-qa`: paired-bootstrap 95% CIs on F1
  deltas, deterministic `-sample-seed` question sampling, failed calls excluded
  (not scored as wrong), per-question distractors instead of one shared passage,
  token-boundary answer matching, and a repro manifest (flags, dataset SHA-256,
  seed) embedded in output. `msc-bench`: held-out best-gate reporting alongside
  the in-sample optimum, validated probe namespaces, `-rewrite-key` for
  authenticated rewrite endpoints. `msc-eval`: `-study-seed`/`-study-n`/
  `-study-folds`; `-compare` reports cross-seed mean and std of held-out F1.
- **Docs corrected against code.** SECURITY.md had MITM scoping backwards (no
  `--mitm-host` means intercept-all); stale thresholds, sweep plateaus, and fuzz
  counts fixed; eval claims right-sized with provenance and limitations notes
  (N=20 tables are directional; per-model deltas need N >= 100).
- **The tag, the version, and the notes are checked against each other.**
  The tag is the only place the version number lives (`make build` stamps it
  from `git describe`), and a tag can be pushed with no `CHANGELOG.md` section
  for it, a version heading with no link, or sections out of version order,
  none of which any existing check noticed. `make check-release` (in `make
  check`) reports each of those, and a `Release notes` CI job runs it on a `v*`
  tag push with `TAG` set, so a release cannot ship undocumented.

### Fixed

- **Breaking: `MSC_WS_DEBUG=0` no longer turns the WebSocket probe on.** The
  switch was read as "set means on", so a shell profile that exported the
  variable to disable it kept the probe logging. `0`, `false`, `off`, and `no`
  now turn it off, as does leaving it empty; any other value turns it on.
- **A memory server's rejection text no longer reaches the log unscrubbed.**
  `muninn_remember` and `muninn_remember_batch` failures are reported at error
  level with the concept and content deliberately omitted, but the error itself
  carried the server's own message, which quotes the memory it refused. The
  text is now scrubbed of direct identifiers and capped before it becomes an
  error value, so no call site can leak captured conversation into a log line, a
  stderr warning, or an `err` field by way of the transport.
- **The store queue is bounded by bytes, not only by slot count.** Its
  back-pressure was a 256-slot channel, but an exchange carries a captured
  request that repeats the whole conversation (up to 50 MiB a body) plus its
  response, so a queue a few slots short of full could be holding tens of GiB
  and take the process down before the depth said anything was wrong. Producers
  now reserve each exchange's body bytes against a 256 MiB budget as they
  enqueue, the worker returns them once the exchange is formatted, and a capture
  that would pass the budget is dropped exactly as one that finds a full queue
  is. `GET /__msc/health` reports `bytes_in_flight`, `bytes_capacity` and
  `bytes_saturated` next to the depth, so a queue dropping on its memory budget
  no longer reads as healthy.
- **A URL with no `//` after the scheme no longer logs its credentials.** The
  log sanitizer redacted userinfo held in `url.Userinfo`, but a URL parsed in
  the opaque form (`https:user:pass@host/v1`, which a client can put in an
  absolute-form request target) keeps that userinfo as literal text inside
  `Opaque`, so the password reached the logs and the stored exchange verbatim.
  Both carriers are redacted now, and a fuzz target holds the contract.

- **The MITM root pool is no longer written into a live transport.**
  `SetMITMRoots` stored the pool by assigning `RootCAs` on the MITM transport's
  `TLSClientConfig`, which `http.Transport` clones on every dial from its own
  goroutines and the upgrade splice clones per request: a pool swapped while the
  proxy was serving raced those clones. The pool is now held under a lock and
  applied through the transport's `DialTLSContext`, so it can be swapped at any
  time; `TestSetMITMRootsConcurrentWithDialing` fails under `-race` without the
  lock.
- **Non-ASCII text no longer breaks at a character boundary.** The `msc-eval`
  and `msc-qa` report tables clipped a scenario name or model label by byte, so
  a multi-byte character (CJK, emoji) was cut in half and the row ended in a
  replacement character; both count characters now. A capped text delta in the
  SSE and WebSocket paths trimmed bytes from the end until the string decoded,
  which discarded every character after a stray invalid byte in the delta rather
  than just the incomplete tail; only the tail is dropped now. The bounded
  capture buffer kept the last N bytes of a stream, which can start part-way
  through a character, so its contents could no longer decode; it now advances
  to the first whole character. A model name in the session summary was the one
  cap still cut by byte, which left invalid UTF-8 in both the tracked name and
  the line that prints it.
- **A Unicode host is refused when minting a certificate.** SNI and CONNECT
  carry punycode, and a certificate SAN cannot hold a non-ASCII name, so
  `--mitm-host` or a CONNECT naming `münchen.de` failed deep inside x509 (or,
  when only interception was scoped, silently blind-tunneled the host). The
  error now names the punycode form to use, and a host with an explicit DNS
  root (`api.openai.com.`) mints the same leaf as one without.
- **Windows stopped getting Unix-only behavior.** A run from a drive root named
  the vault `\` instead of falling back to `sidecar`; the token file and MITM CA
  key warned about "overly permissive permissions" on every run, because Go
  reports 0666 for any writable file there and the suggested `chmod` only
  toggles the read-only attribute; `msc ca` printed POSIX `export` lines with
  an unquoted `%AppData%` path that cannot be pasted into cmd or PowerShell;
  and `--mitm` pointed `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/`CURL_CA_BUNDLE`
  at msc's CA alone, replacing the child's entire trust store, because Windows
  keeps its roots in the OS certificate store and has no system PEM bundle to
  combine with. Those variables are now left unset when no combined bundle
  exists, the additive `NODE_EXTRA_CA_CERTS`/`DENO_CERT` still carry the CA,
  and `msc ca` says where Windows keeps its roots instead of naming a bundle
  that is never written.
- **`msc-qa -md` grew a new results block on every run against a CRLF file.**
  The manifest marker was compared with the line's CR still attached, so a
  report edited on Windows (or checked out with `core.autocrlf`) never matched
  its own marker and each rerun appended another copy. Markers now match with
  either line ending, and the block is written with the endings the file
  already uses.
- **`msc-bench -seed` no longer doubles the corpus on a rerun.** The seeded
  memories carried no `dedup_key`, so re-seeding the same corpus stored a second
  copy of every item: the duplicates crowd recall's top-k and skew the retrieval
  numbers the benchmark exists to measure. Every seeded memory now carries the
  same content-addressed key `msc` and `msc-qa` send.
- **Breaking: bad endpoint config fails at startup.** `--ground-url` (and
  the eval binaries' `-ground-url`, `-model-url`, `-rewrite-url`) accepted any
  string, so a typo failed per request as a transport error or silently left the
  grounder off. All of them now go through one `config.ValidateURL` check, the
  same fail-fast gate `-mcp-url` already had. `--ground-model`, `--ground-topk`,
  and `--ground-timeout` are also rejected without a
  `--ground-url`/`--ground-cmd` backend instead of being silently dropped, and
  `msc-qa`/`msc-eval` reject a `-min-score` outside (0,1] (NaN included), which
  would otherwise inject everything or nothing and misreport the run.
- **The MuninnDB default endpoint had three definitions.** `msc-bench` and
  `msc-qa` each carried their own copy of `http://127.0.0.1:8750/mcp` while
  `msc-eval` resolved it through `config`; the dev binaries now take the shared
  default, so one edit covers all of them.
- **`--debug` logs the effective configuration.** A captured session log now
  names the endpoint, vault, injection budget/gate/recall mode, redaction, and
  MITM state. The bearer token is reported as set/unset, never by value.
- **Breaking: `msc help <typo>` no longer looks like it worked.** It ignored
  the topic, printed the global usage, and exited `0`, so a mistyped topic in a
  script passed silently. An unknown topic now reports itself, suggests the
  nearest one, and exits `2`; more than one topic is a usage error too.
- **Breaking: a missing agent binary exits `127`, not `1`.** `msc <agent>` for
  an agent that is not installed reported "not found in PATH" and exited `1`,
  the same code it used for a general runtime failure. It now uses the shell's
  "command not found" code, so a script can tell a mistyped agent name apart
  from an agent that ran and failed. The exit-code table is documented in
  `msc --help` and the README.
- **The shipped binary no longer depends on the build host's libc.** Nothing
  here imports `C`, but the builds ran with cgo on, so `msc` linked against
  whatever glibc the build machine had, needed a working C toolchain to build
  at all, and produced different bytes on different hosts. `make build`,
  `make build-all` and `make install` now set `CGO_ENABLED=0` and emit static
  binaries.
- **The build date stamp silently went back to wall-clock time on macOS.** The
  `date -d @N` fallback is GNU-only, so on the BSD `date` every macOS in the
  release matrix has, the second candidate failed too and the stamp fell
  through to `date -u` with no argument. The Makefile now tries the BSD
  spelling as well, and reports `unknown` rather than a moving date if neither
  works.
- **CI never built with the flags that ship.** The `Build` steps ran a bare
  `go build -o /dev/null ./...`, so `-trimpath`, `-buildvcs=false` and the
  version stamp were untested there, and the same command was written out three
  more times across the workflow. CI now calls `make build-all`, `make vet`,
  `make tidy-check`, `make fmt-check`, `make test` and `make lint-go`, so the
  Makefile is the only place the build is described.
- **The reproducibility job could fail for the wrong reason.** It built a copy
  of the tree and compared hashes, but both builds ran `git describe` in a
  directory whose index had just been copied. The job now pins `VERSION`,
  `COMMIT` and `SOURCE_DATE_EPOCH`, so a hash mismatch means a leaked path or
  timestamp and nothing else.
- **CLI-agent backends no longer leak output buffers and orphan grandchildren.**
  The grounding judge capped its child's stdout and killed the child's process
  group on timeout; the query rewriter (`msc-bench`) and the answer client
  (`msc-qa`) did neither, so a runaway agent grew the parent heap without
  limit and a timeout left the agent's own helpers running. All three now share
  one runner (`internal/clirun`) that caps the captured output and signals the
  whole process group.
- **Short-lived MCP clients no longer strand their connections.** Every
  `mcpclient.New` owns a private `http.Transport`, and a fully-read response
  returns its connection to that transport's idle pool. Callers that build a
  client per call (`msc status`, the startup health check) dropped it with the
  socket still parked, so the connection and its read/write goroutines lived
  until the process exited. `Client.Close` releases them, and the store,
  injector, and one-shot callers now call it.
- **Builds are reproducible.** `make build` passed no `-trimpath`, so the
  checkout path was embedded in every binary, and the stamped build date came
  from the wall clock, so no two builds of one commit matched. Builds now use
  `-trimpath -buildvcs=false` and derive the date from `SOURCE_DATE_EPOCH`
  (defaulting to the commit timestamp). A new `Reproducible build` CI job
  builds twice from two directories and compares hashes.
- **ndjson streams are captured.** Stream capture only read SSE lines, so a
  server that streams bare JSON events (an ndjson body, no `data: ` prefix) was
  forwarded but never captured. A line starting with `{` is now treated as an
  event payload; SSE control lines (`event:`, `id:`, `retry:`, `:`) still carry
  none and are skipped.
- **WebSocket assistant text is bounded.** The per-turn assistant text
  accumulated from decoded WebSocket deltas had no cap, so one long turn grew
  without limit. Deltas are now clamped to `wsMaxRespText` as they arrive (the
  clamp never lands mid-rune), so the turn is stored truncated rather than
  buffered whole.
- **`--dry-run` shows what the child actually gets.** The preview built its own
  env map instead of calling the same helpers `Exec`/`ExecMITM` use, so it could
  disagree with the launch: flag-based agents (`qwen`) showed only env overrides
  and no argv, and under `--mitm` it advertised `SSL_CERT_FILE` pointing at the
  CA alone, which would break every other TLS connection from that shell. The
  preview is now built from the launch helpers. `--dry-run --json` gains
  `proxy_args`, `inject_min_score`, `inject_recall_mode`, and
  `inject_calibration`, and its `env` values are the real ones. `msc ca` points
  at the combined system+msc bundle for the same reason.
- **Breaking: `--inject-min-score nan` is rejected.** The range test was
  `f <= 0 || f > 1`, which `NaN` passes, so a NaN threshold reached the injector.
  The test is now a positive check, `--inject-min-score must be in (0,1]`.
- **A full MuninnDB queue no longer floods the log.** Every dropped exchange
  logged a warn line, so a sustained MuninnDB outage produced one line per agent
  turn and buried everything else. The first drop and every 100th after it are
  logged with a running total; `Stats.Dropped` carries the exact count.
- **The session window has a stable order.** The window is built from a map, so
  memories with equal effective scores were emitted in a different order on
  every run. Ties now break on ID, so the injected block is replayable.
- **The grounding judge no longer leaks a connection per turn.** `--ground-url`
  built a fresh `http.Client` and `http.Transport` inside every `Relevant` call,
  and that transport sets no idle-connection timeout, so each grounded turn
  stranded its keep-alive socket (and its read/write goroutines) on a transport
  nothing could close. One client is now built per grounder and reused, with a
  bounded idle pool. The CLI judge gained the matching bounds: its output is
  captured into a capped tail buffer instead of an unbounded `bytes.Buffer`,
  and a judge that times out is killed as a process group, so the helpers it
  spawned do not outlive the call.
- **A signalled agent's own children go down with it.** Signal forwarding
  reached only the agent's pid, so helpers it had spawned survived msc's exit
  (most visibly after the 3s SIGKILL fallback) and kept running against a proxy
  that was about to close. The agent's process group is signalled instead,
  except when that is the group msc itself belongs to, which also holds the
  user's shell.
- **The model breakdown stops growing without a ceiling.** Model names are read
  out of captured request bodies, so every distinct string a client ever sent
  became a permanent session counter, and one long name was retained verbatim.
  Names are now capped in count and length, and the captures past the cap are
  reported as `other (N)` in the summary rather than dropped silently or, worse,
  leaving the list looking complete.
- **Direct identifiers no longer reach third parties on the read path.** The
  recall query is scrubbed before it is sent to MuninnDB, and
  `grounding.Prompt` scrubs the query and candidate passages before they reach
  a judge model. Both paths previously carried unredacted text off-process.
- **Signals reach the agent without a `/proc`.** Child discovery walked
  `/proc`, so on macOS, Windows, or a locked-down `/proc` a `kill`/`docker stop`
  aimed at msc left the agent running against a dead proxy, and the 3s SIGKILL
  fallback had nothing to signal. msc now publishes the agent process handle
  and falls back to it, still leaving SIGINT to the kernel so Ctrl+C is not
  double-delivered. The Unix-only `syscall.Kill`/`syscall.Getpgrp` calls moved
  behind `//go:build` shims, so `GOOS=windows go build` compiles again.
- **No hardcoded `/tmp`.** `msc-bench`/`msc-qa` `-squad-file` and
  `scripts/fetch_hf_datasets.py` resolve their default dataset path through
  `os.TempDir()` / `tempfile.gettempdir()`.
- **A retried memory write is one write.** A MuninnDB flush that fails after the
  server committed it was retried under a fresh JSON-RPC id with no dedup
  token, so the memory was stored twice. Every attempt of a flush now reuses one
  request id, and each memory carries a content-addressed `dedup_key`
  (SHA-256 of vault + concept + content).
- **`msc-qa -md` converges on rerun.** Results were appended blindly, so a
  repeated run with identical flags added a second copy of the same rows. Each
  run's block is now keyed by its repro manifest and replaced in place; a
  different configuration still gets its own block. Rows are also written once
  at the end, so a run that dies mid-way leaves no truncated block.
- **Upgrades survive capture.** 101 Switching Protocols responses are no longer
  consumed by capture on the plain proxy path; WebSocket upgrades pass through
  intact.
- **Tunnels drain both directions.** `blindTunnel` and the WebSocket splice
  waited on the first copy direction only, tearing down the peer mid-transfer;
  both now finish (half-close forwarded through the conn wrappers). CONNECT
  bytes pipelined behind the header (early TLS ClientHello) are kept, and
  header-block reads are bounded even without a newline.
- **permessage-deflate stays in sync.** Skipped compressed WebSocket messages
  now advance the inflater dictionary instead of desyncing context takeover.
- **Gemini Cloud Code output is captured.** The `{"response":{...}}` envelope is
  unwrapped for assistant/SSE extraction; user-query extractors scan back past
  tool-result and text-free turns, and an omitted role defaults to `user`.
- **Grounding rejections stick.** Judge-rejected memories are evicted from the
  session window, so tool-use continuations no longer re-inject them. Verdict
  parsing requires a word boundary ("3 notes" no longer reads as "3: no"), and
  the CLI grounder prompt travels over stdin instead of argv (was visible in
  the process table).
- **Redaction corrects both directions.** `sk-`/`xai-` patterns are word-anchored
  (no more mangled hyphenated identifiers), scp-style remotes are no longer
  eaten as emails while `email:password` combos now are redacted, multi-word and
  unterminated quoted values are fully scrubbed, and redaction is idempotent.
- **Scoped MITM keeps TLS working.** `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/
  `CURL_CA_BUNDLE` now point at a combined system+msc bundle instead of
  replacing the child's entire trust store; a mismatched CA cert/key pair is
  detected and regenerated.
- **No orphaned agents.** SIGINT/SIGTERM to msc is forwarded to the agent child
  (SIGKILL after 3s), without double-signaling on terminal Ctrl+C; msc exits
  128+signum for signal-terminated children.
- **Nesting resolves the right upstream.** The upstream sentinel is scoped per
  agent (`MSC_UPSTREAM_<AGENT>`), and a nested msc detects a base-URL var
  poisoned by a different agent's parent proxy. Gemini-mode `countTokens` no
  longer stores duplicate user-only memories.
- **Hijacked tunnels no longer block forever.** A CONNECT tunnel and a spliced
  protocol upgrade each waited, unbounded, on a peer that had already been
  reached: the client-side TLS handshake and the backend's reply to the upgrade
  request. After `Hijack` the http.Server no longer owns the connection, so a
  stalled or silent peer pinned the serving goroutine and both sockets with no
  way to recover. Both reads are now bounded (30s, cleared once the handshake
  completes) and tests pin the bound.
- **The store's preparer is synchronized.** The capture-side `Preparer` was a
  plain field written by `SetPreparer` while the worker goroutine (already
  running) read it; it is now guarded, so the preparer can be swapped while
  captures flow.
- **`msc-bench` and `msc-qa` build again.** Both used `filepath.Join` without
  importing `path/filepath`, so the two commands failed to compile.

## [0.4.4] — 2026-06-02

### Fixed

- **OpenAI Responses capture no longer discards the user's `instructions`.** When
  stripping the injected context block from a captured OpenAI Responses request,
  the code located the block by the *last* blank line (`LastIndex("\n\n")`) — but
  the block itself contains blank lines, so a multi-memory block truncated at an
  internal one and the user's original system `instructions` were dropped from
  the stored memory. The block is now located by its opening marker, restoring
  the full original instructions. (Capture-side only; forwarded requests were
  never affected.)

## [0.4.3] — 2026-06-01

### Changed

- **Skip gRPC responses during capture.** `application/grpc` responses (e.g.
  agy's cloudcode-pa inference) are length-prefixed protobuf the extractors
  can't read; msc now skips capturing them outright (the response still forwards
  untouched) instead of buffering binary that would extract to nothing. Lays the
  groundwork for proper protobuf support later.

## [0.4.2] — 2026-06-01

### Fixed

- **No false "unparseable response body" warnings.** `extractModelAndTokens`
  logged a debug warning whenever a captured response wasn't a JSON object —
  including `buildRespBody`'s legitimate string fallback for a stream with no
  structured final event (seen live with qwen/DeepSeek). Valid non-object bodies
  now return silently; only genuinely malformed JSON is flagged.

### Changed

- **agy capture status documented accurately.** Live probing confirmed `--mitm`
  intercepts agy's HTTPS (auth/register/userinfo), but agy's inference runs over
  gRPC/protobuf on cloudcode-pa, which the JSON extractors can't read — so turns
  aren't captured in usable form (full support needs protobuf decoding). The agy
  agent comment, README footnote, and `docs/websocket-agents.md` now say so.

## [0.4.1] — 2026-06-01

### Added

- **qwen captures the Gemini API format too.** Qwen Code is a Gemini-CLI fork; in
  its Google auth mode it speaks the Gemini API (`:generateContent` /
  `:streamGenerateContent`), not just the OpenAI-compatible DashScope endpoint.
  Added the Gemini capture paths so qwen turns are captured in either mode.

### Removed

- **`reasonix` agent.** Dropped from the registry and docs.

### Fixed

- **grok conversations are now captured.** The grok CLI's default subscription
  mode sends inference to `cli-chat-proxy.grok.com` via the OpenAI Responses API
  (`POST /v1/responses`) over HTTPS — not `/chat/completions` and not a
  WebSocket. That endpoint was missing from grok's capture paths, so turns ran
  through `--mitm` uncaptured (`capture=false`). Added `/responses` to the
  shared `/v1`-base capture paths; grok turns are now captured, stored, and
  recalled. Verified live end-to-end.
- **opencode captures Anthropic-format turns.** opencode's provider-agnostic
  "zen" backend routes some models via the OpenAI Chat Completions API and
  others via the Anthropic Messages API (`/zen/v1/messages`); only the former
  was in its capture paths. Added `/v1/messages` so both formats are captured
  regardless of which model is selected.

## [0.4.0] — 2026-06-01

### Added

- **`MSC_WS_DEBUG` WebSocket protocol probe.** When set, the MITM upgrade-splice
  logs the JSON envelope `type` and byte size of every decoded WebSocket message
  per direction (`dir=c->s` / `dir=s->c`) — the message *shape*, never its
  content. msc decodes codex's Responses-API WebSocket out of the box; this flag
  surfaces the envelope of other agents' proprietary WebSocket protocols (e.g.
  grok's gateway, `wss://grok.com/ws/gw/`) so a new protocol can be mapped and
  handled alongside codex. Off by default.
- **`docs/websocket-agents.md`** — records a static probe of installed agents'
  LLM transports: only codex streams a capturable WebSocket today; grok's
  gateway mode is a future target; `agy` is gRPC, not WebSocket; the rest are
  HTTP/SSE on the normal path.

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

[unreleased]: https://github.com/maci0/muninn-sidecar/compare/v0.4.4...HEAD
[0.4.4]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.4.4
[0.4.3]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.4.3
[0.4.2]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.4.2
[0.4.1]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.4.1
[0.4.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.4.0
[0.3.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.3.0
[0.2.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.2.0
[0.1.0]: https://github.com/maci0/muninn-sidecar/releases/tag/v0.1.0
