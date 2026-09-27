# Testing & Fuzzing

Every parsing and transform surface in the tree has a Go fuzz target, and every
package carries high statement coverage (the floor is `cmd/msc`, where the
process-level paths are covered by a re-exec test rather than in-package ones).

## Coverage

- High statement coverage across every package: internal **~84–100%**
  (`redact` 100%, `grounding` 99%, `store` 97%, `stats` 96%, `inject` 96%,
  `config` 93%, `agents` 91%, `proxy` 92%, `apiformat` 90%, `mcpclient` 89%,
  `mitm` 84%), cmd **66–91%** (`msc` 66% — its `main()` paths are covered by a
  re-exec test, not by in-package tests — and `msc-qa`/`msc-bench` 91%/90%).
- `make cover` — race-enabled coverage with a per-function breakdown.
- The `main()` wrappers are exercised via a re-exec test (`TestMainHelp`)
  that runs `main()` inside the instrumented test binary, so even those count.
- Functions needing external services (recall/store, agent exec, model/grounder
  calls) are tested with `httptest` fakes; the agent launcher's lookup/error path
  is tested with a missing binary and the success path with `/bin/true`.

## Fuzzing

61 fuzz targets cover the untrusted-input surfaces:

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
- **mitm** — CONNECT host normalization (`normalizeHost`) and per-host leaf
  minting (`LeafFor`).
- **mcpclient** — health URL derivation, and the response-classification
  boundary (`classifyResponse`: 5xx/4xx/JSON-RPC error, body passed through
  unaltered on success).
- **store** — captured-exchange format + dedup pipeline, and the MCP retry
  decision (`retryable`: 4xx and JSON-RPC errors permanent, 5xx and transport
  failures transient, verdict stable under wrapping).
- **cmd/msc** — flag parsing, Levenshtein, closest-match.
- **cmd/msc-bench** — recall parse, query transforms, string/number helpers
  (`itoa`), corpus generators, query-rewrite sub-query parsing + prompt build.
- **cmd/msc-qa** — generic QA loading, SQuAD-style answer scoring, CLI-reader
  prompt build / last-line extraction, entity-span split.

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

`make check` runs the full CI `test` job locally (tidy, gofmt, vet,
staticcheck, `go test -race`, build) so nothing waits for a push to fail.
`make fuzz` and `make vuln` are the two CI jobs that stay separate, being slow
and network-bound.
