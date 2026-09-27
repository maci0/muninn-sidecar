# Contributing to `msc` (muninn-sidecar)

Thanks for contributing. This guide covers the workflow and the project's
quality bar so a change lands cleanly.

## Prerequisites

- **Go 1.25 or newer** (pinned in `go.mod`; CI tracks the latest 1.25.x patch).
- **staticcheck and govulncheck**, which CI installs and runs, so `make check`
  and `make vuln` require them too:

  ```sh
  make tools   # go install both into GOBIN (no sudo, no system packages)
  ```

  Pin versions with `make tools STATICCHECK_VERSION=v0.6.0
  GOVULNCHECK_VERSION=latest` if you need to match a specific linter release.

`make help` lists every target. There are no other system dependencies: no
database, no C toolchain, no services to start for a build or test run.

## Build & run

```sh
make build              # build all binaries (msc, msc-bench, msc-eval, msc-qa)
make install            # go install ./cmd/msc
go run ./cmd/msc status # quick smoke against a local MuninnDB
```

`msc` needs [MuninnDB](https://github.com/scrypster/muninn) reachable (default
`http://127.0.0.1:8750/mcp`, override with `MUNINN_MCP_URL`). The tests use
`httptest` fakes, so no MuninnDB is needed to run them.

Builds are reproducible: the same source at the same commit produces
bit-identical binaries regardless of checkout path, wall-clock time, or locale.
`make build` passes `-trimpath` and `-buildvcs=false`, and stamps the build date
from `SOURCE_DATE_EPOCH`, which defaults to the commit's own timestamp. Set it
explicitly to build as of a different point in time:

```sh
SOURCE_DATE_EPOCH=1700000000 make build
```

The `Reproducible build` CI job builds twice from two different directories and
compares the hashes, so a regression here fails the build.

## The edit-test loop

`make test` runs the whole tree under `-race` (about 25s). Narrow it to what you
are editing:

```sh
make test PKG=./internal/redact            # one package, still -race
make test PKG=./internal/redact RUN='^TestFoo$'  # one test
make test-fast PKG=./internal/inject       # same, without -race
```

## Before you open a PR

One command runs everything the CI `test` job runs, in the same order:

```sh
make check   # tidy-check, fmt-check, lint (go vet + staticcheck + shellcheck + ruff + yamllint), go test -race, build
```

The other two CI jobs are separate because they are slow or need the network:

```sh
FUZZTIME=8s make fuzz   # brief campaign over every fuzz target
make vuln               # govulncheck against the Go vulnerability DB
make cover              # coverage report
```

The module has **no third-party dependencies** (standard library only) — keep it
that way unless there's a compelling reason; it's the project's strongest
supply-chain property. `make vuln` (and CI) then mainly guards stdlib CVEs.

### Quality bar

- **Every function has a test, and every parsing/transform surface has a fuzz
  target.** New exported behavior ships with both. Fuzz targets assert
  invariants (no-panic is the floor; prefer real properties — idempotence,
  round-trip, UTF-8 validity, bounds).
- **`-race` clean.** Shared state uses `sync`/`sync/atomic`; the store worker is
  single-goroutine by design.
- **gofmt + `go vet` + staticcheck clean.** No new warnings. The non-Go files
  are held to the same bar: `ruff` (`ruff.toml`) for `scripts/*.py`, `shellcheck`
  for `test-live.sh`, `yamllint` (`.yamllint.yml`) for the workflow YAML. CI runs
  all of them; `make lint` runs whichever are installed locally and names the
  ones it skipped.
- **Keep behavior verified, not assumed.** When a change depends on an external
  contract (a MuninnDB tool's response, an agent's env var), verify it against a
  live instance and add a regression guard.

## Adding an agent

Agents live in `internal/agents/agents.go` (`Registry`). Each entry maps a CLI
to how its API traffic is intercepted (`EnvKey`/`ExtraEnvKeys` for base-URL
overrides, `ProxyArgs` for flag-based agents like qwen, `CapturePaths`).

**Verify empirically before adding an entry.** The base-URL env var (or flag)
and capture paths were each confirmed by running the real agent against a local
probe server and watching what it sends — guessing leads to entries that
silently capture nothing. If an agent ignores base-URL overrides (OAuth-direct,
WebSocket), it needs `--mitm`; document the caveat in the README agent table.

## Secret hygiene

- Redaction patterns (`internal/redact`) are deliberately **conservative** —
  anchored to distinctive provider prefixes/structures or sensitive key names —
  to avoid corrupting prose. Add patterns the same way; include a no-false-
  positive test case.
- **Never commit a real-looking secret**, even in tests — GitHub push protection
  will (correctly) block it. Build secret-shaped test fixtures from runtime
  fragments (`"sk-" + strings.Repeat("a", 30)`) so no contiguous secret literal
  sits in source.

## TLS-MITM

`--mitm` is opt-in. The local CA's private key is generated on the machine,
stored `0600`, and trusted only by the launched child (via env), never the
system trust store. Keep it that way. The decrypted pipeline must forward bytes
faithfully — any capture/decode logic runs best-effort so it can never break the
agent's connection (see `internal/proxy/mitm.go`).

## Commits & changelog

- **Conventional commits**: `type(scope): summary` (`feat`, `fix`, `refactor`,
  `test`, `docs`, `chore`). Explain the *why* in the body.
- **No AI attribution** in commit messages or trailers.
- Update `CHANGELOG.md` under `[Unreleased]` for user-visible changes; versions
  follow SemVer and tag from `main`.
