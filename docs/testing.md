# Testing & Fuzzing

Every function in the tree has test coverage, and every function that ingests
fuzzable input (bytes / strings / numbers — i.e. all parsing and transform
surfaces) has a Go fuzz target.

## Coverage

- **0 functions at 0% coverage**; ~85% statement coverage overall.
- `make cover` — race-enabled coverage with a per-function breakdown.
- The four `main()` wrappers are exercised via a re-exec test (`TestMainHelp`)
  that runs `main()` inside the instrumented test binary, so even those count.
- Functions needing external services (recall/store, agent exec, model calls)
  are tested with `httptest` fakes; the agent launcher's lookup/error path is
  tested with a missing binary and the success path with `/bin/true`.

## Fuzzing

29 fuzz targets cover the untrusted-input surfaces:

- **apiformat** — request/response extraction, recent-context, system-reminder
  strip, truncation, SSE delta/tool-name.
- **inject** — recall/where-left-off/guide/MCP-text parsers, `InjectContext`,
  selection + budget packing, live-scenario parse, metric primitives, Otsu
  calibration, nDCG.
- **proxy** — request/response anti-recursion filtering, SSE parsing, injected-
  context stripping.
- **mcpclient** — health URL derivation.
- **cmd/msc** — flag parsing, Levenshtein, closest-match.
- **cmd/msc-bench** — recall parse, query transforms, string/number helpers,
  corpus generators.
- **cmd/msc-qa** — generic QA loading, SQuAD-style answer scoring.

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
