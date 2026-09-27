# Testing & Fuzzing

Every parsing and transform surface in the tree has a Go fuzz target, and every
package carries high statement coverage (the floor is `cmd/msc`, where the
process-level paths are covered by a re-exec test rather than in-package ones).

## Coverage

- High statement coverage across every package: internal **~85–100%**
  (`querysplit`/`reqid`/`strhash` 100%, `store` 99%, `redact` 97%, `stats` 96%,
  `inject` 96%, `clirun` 96%, `config` 96%, `grounding` 95%, `proxy` 92%,
  `agents` 91%, `apiformat` 90%, `mcpclient` 89%, `mitm`/`clock` 86%,
  `tailbuf` 86%), cmd **82–90%**
  (`msc` 83% — its `main()` paths are covered by a re-exec test, not by
  in-package tests — and `msc-bench`/`msc-qa`/`msc-eval` 90%/89%/86%).
  Measured with `go test -count=1 -cover ./internal/... ./cmd/...`.
- `make cover` — race-enabled coverage with a per-function breakdown.
- The `main()` wrappers are exercised via a re-exec test (`TestMainHelp`)
  that runs `main()` inside the instrumented test binary, so even those count.
- Functions needing external services (recall/store, agent exec, model/grounder
  calls) are tested with `httptest` fakes; the agent launcher's lookup/error path
  is tested with a missing binary and the success path with `/bin/true`.

## Fuzzing

67 fuzz targets cover the untrusted-input surfaces:

- **apiformat** — request/response extraction, recent-context, system-reminder
  strip, truncation (UTF-8 + length invariants), SSE delta/tool-name.
- **inject** — recall/where-left-off/guide/MCP-text parsers, `InjectContext`,
  selection + budget packing, live-scenario parse, metric primitives, Otsu
  calibration, top-Z, nDCG.
- **redact** — secret redaction (idempotence, marker-on-change).
- **grounding** — listwise verdict-mask parsing (`ParseMask`) and grader-prompt
  build (`Prompt`).
- **agents** — proxy-flag argument injection / `{proxy}` substitution
  (`buildArgs`) and the TLS-MITM child environment (`BuildMITMEnv`).
- **proxy** — request/response anti-recursion filtering, SSE parsing, injected-
  context stripping, model/usage extraction, JSON sanitizing, synthetic response
  building, MITM helpers (`stripPort`, `isUpgradeRequest`,
  `shouldInterceptHost`), and the WebSocket-capture path: frame decoding
  (`readWSFrame`), permessage-deflate inflation (`inflate`), the 101 header
  reader, and the codex message-pairing parsers.
- **mitm** — CONNECT host normalization (`normalizeHost`), per-host leaf
  minting (`LeafFor`), and CA re-parse (`ParseCA`).
- **mcpclient** — health URL derivation, and the response-classification
  boundary (`classifyResponse`: 5xx/4xx/JSON-RPC error, body passed through
  unaltered on success).
- **store** — captured-exchange format + dedup pipeline, and the MCP retry
  decision (`retryable`: 4xx and JSON-RPC errors permanent, 5xx and transport
  failures transient, verdict stable under wrapping).
- **querysplit** — entity-span decomposition of a question into sub-queries
  (`Split`: the full query first, then each capitalized run).
- **cmd/msc** — flag parsing, Levenshtein, closest-match.
- **cmd/msc-bench** — recall parse, query transforms, string/number helpers
  (`itoa`), corpus generators, query-rewrite sub-query parsing + prompt build.
- **cmd/msc-qa** — generic QA loading, SQuAD-style answer scoring, recall-payload
  parsing, CLI-reader prompt build / last-line extraction.

Run all of them briefly (regression smoke):

```sh
make fuzz            # ~5s per target
FUZZTIME=60s make fuzz   # longer campaign
```

Fuzz-discovered crashers are saved under each package's `testdata/fuzz/` and
re-run on every `go test`, so they become permanent regressions. Fuzzing has
already hardened real code (e.g. `truncateAt` against a non-positive limit) and
corrected over-strict invariants (non-JSON passthrough, float64 number overflow).

## Everyday

```sh
make test    # race, all packages
go test ./...
```

While iterating, narrow the run to the package or test being edited instead of
paying for the whole tree under `-race`:

```sh
make test PKG=./internal/inject                    # one package, still -race
make test PKG=./internal/inject RUN='^TestSelect$' # one test
make test-fast PKG=./internal/inject                # no -race, quicker
```

`make check` runs the full CI `test` job locally (tidy, the Go version against
`go.mod`, gofmt, vet, staticcheck, `go test -race`, `make build-all`,
`make check-release`) so nothing waits for a push to fail. It also stops when
shellcheck, ruff or yamllint is
missing, because CI installs all three and fails without them.
`make fuzz` and `make vuln` are the two CI jobs that stay separate, being slow
and network-bound; the cross-platform `build` matrix is `make build-matrix`.
The `Release notes` job runs only on a `v*` tag, and `make check-release
TAG=vX.Y.Z` runs the same check before the tag is pushed.

The `-race` targets need a C compiler on `PATH` (`go test -race` links the race
runtime through cgo). `make doctor` checks that, the Go version against
`go.mod`, and which CI linters are installed; `make test-fast` is the escape
hatch on a machine without one.

## Live end-to-end

`go test` never touches a real provider: the proxy, the MCP client and the
store all run against `httptest` fakes. `test-live.sh` is the other half, the
only check that runs the whole path against a live MuninnDB and real agent
CLIs. CI does not run it, since it needs both, so it runs only on demand:

```sh
./test-live.sh        # -d also passes -d through to msc, for the child's logs
```

What it needs, all checked at startup so a missing tool is a clear failure
rather than a cascade of empty result sets:

- `curl`, `jq`, `make` and `go` on `PATH`.
- A MuninnDB on `MUNINN_MCP_URL` (default `http://127.0.0.1:8750/mcp`) and its
  token in `MUNINN_TOKEN_FILE` (default `~/.muninn/mcp.token`).
- The agent binaries for the agents it drives (`claude`, `qwen`, `codex`) on
  `PATH`; each one it cannot launch is reported as a failed run, not skipped.

It captures into a throwaway vault named `msc-test-$$`, asserts the stored
memories (no `system-reminder` leakage, no `count_tokens` captures, no
duplicate concepts, at least one memory) and checks that one agent recalls a
memory written by another. The `EXIT` trap deletes that vault's memories and
the 600-mode header file holding the token, so nothing survives the run. Tune
`SETTLE_SECONDS`, `CURL_CONNECT_TIMEOUT` and `CURL_MAX_TIME` if your MuninnDB
is slow or remote; the curl deadlines are what stop a dead server from hanging
the run instead of failing it.
