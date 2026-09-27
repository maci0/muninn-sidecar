# Contributing to `msc` (muninn-sidecar)

Thanks for contributing. This guide covers the workflow and the project's
quality bar so a change lands cleanly.

## Prerequisites

- **Go 1.25 or newer** (pinned in `go.mod`; CI tracks the latest 1.25.x patch).
- **A C compiler**, for the race detector. No package in this module imports
  `"C"`, so the binaries build without one (`CGO_ENABLED=0`, see Build & run),
  but `go test -race` links the race runtime through cgo, so `make test`,
  `make check` and `make cover` need a working `$(go env CC)`. `make test-fast`
  has no `-race` and needs nothing.
- **staticcheck and govulncheck**, which CI installs and runs, so `make check`
  and `make vuln` require them too:

  ```sh
  make tools   # go install both into GOBIN (no sudo, no system packages)
  ```

  Pin versions with `make tools STATICCHECK_VERSION=v0.6.0
  GOVULNCHECK_VERSION=latest` if you need to match a specific linter release;
  `make tools-staticcheck` and `make tools-govulncheck` install just one of
  them, which is what each CI job does.
- **shellcheck, ruff and yamllint**, for `make check`. `make lint` skips any it
  cannot find, but CI installs all three and fails on a finding, so the
  advertised local mirror of that job (`make check`) refuses to run without
  them. `pipx install ruff yamllint`; shellcheck comes from your package
  manager.

```sh
make doctor   # checks every one of the above against this machine
```

Run it first on a new machine: an old Go or a missing compiler otherwise
surfaces as an unrelated-looking build error, or as `-race requires cgo`, which
names the wrong knob. Beyond those tools there are no further dependencies: no
database and no services to start for a build or test run.

## Build & run

```sh
make build              # build all binaries (msc, msc-bench, msc-eval, msc-qa)
make install            # go install ./cmd/msc
go run ./cmd/msc status # quick smoke against a local MuninnDB
```

`msc` needs [MuninnDB](https://github.com/scrypster/muninn) reachable (default
`http://127.0.0.1:8750/mcp`, override with `MUNINN_MCP_URL`). The tests use
`httptest` fakes, so no MuninnDB is needed to run them.

Builds are reproducible: the same source at the same commit, built with the same
Go toolchain, produces bit-identical binaries regardless of checkout path,
wall-clock time, or locale. The Go toolchain is part of that input, not an
assumption: the Go runtime stamps its own version into the binary, so two
patches of the same release line do not produce the same bytes. `msc version`
prints the toolchain the binary was built with, which is how you check two
artifacts before comparing them.

`make build` passes `-trimpath` and `-buildvcs=false`, stamps the build date
from `SOURCE_DATE_EPOCH`, which defaults to the commit's own timestamp, and sets
`CGO_ENABLED=0`, so the result is a static binary that does not vary with the
build host's libc. Set the epoch explicitly to build as of a different point in
time:

```sh
SOURCE_DATE_EPOCH=1700000000 make build
```

`make build-all` compiles every package with those same flags and leaves no
artifacts; it is what CI runs, so a flag added to the Makefile reaches CI on the
next push instead of living in a second copy in `ci.yml`.

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
make check   # tidy-check, fmt-check, lint-available, lint (go vet + staticcheck + shellcheck + ruff + yamllint), go test -race, build-all
```

`lint-available` is the CI parity gate: the non-Go linters are optional for a
bare `make lint`, but `make check` names any that are missing and stops, so a
green local run cannot become a red CI run.

The remaining CI jobs are separate: the cross-platform `build` matrix (windows
and darwin, both arches) and the two slow or networked ones. The matrix is
reproducible locally, without a push:

```sh
make build-matrix       # every GOOS/GOARCH the CI build job covers, in its order
FUZZTIME=8s make fuzz   # brief campaign over every fuzz target
make vuln               # govulncheck against the Go vulnerability DB
make cover              # coverage report
```

Everything above runs on `httptest` fakes. `./test-live.sh` is the one check
that runs the whole path against a live MuninnDB and real agent CLIs, so it
needs both and CI does not run it; see
[docs/testing.md](docs/testing.md#live-end-to-end) for what it drives and what
it needs.

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
  all of them; a bare `make lint` runs whichever are installed locally and names
  the ones it skipped, while `make check` requires all three (see
  `lint-available`).
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

An entry belongs under `[Unreleased]` when the change lands, not when the release
is cut: a shipped feature (the observability work, `request_id` logging,
`--log-json`, `/__msc/health`) accumulated commit after commit with no entry at
all. Group by impact, `Added` / `Fixed` / `Changed`, and write what a user sees
change, not which function changed.

## Release

The project is pre-1.0, so a minor bump may carry breaking CLI or output
changes; say so in the entry rather than assuming SemVer protects the reader.
One commit does all four steps, so the tag, the version, and the notes cannot
drift apart:

1. Rename `## [Unreleased]` to `## [X.Y.Z] — YYYY-MM-DD` in `CHANGELOG.md`,
   leaving an empty `## [Unreleased]` above it.
2. Add `[X.Y.Z]: .../releases/tag/vX.Y.Z` to the link list at the bottom.
3. Tag that commit `vX.Y.Z` on `main`. The tag is the release, and a release
   published without matching notes cannot be corrected afterwards.

There is nothing to bump in the source: `make build` and `make install` pass
`git describe` to `-X main.version`, so the tag is the only place the number
lives. The `version` default in `cmd/msc/main.go` stays `dev`, which is what a
`go build` of a tree past the tag reports. Do not hand-edit it to the new
version: that is how a post-tag commit ends up reporting a release whose
feature set it does not have.
