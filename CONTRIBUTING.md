# Contributing to `msc` (muninn-sidecar)

Thanks for contributing. This guide covers the workflow and the project's
quality bar so a change lands cleanly.

## Build & run

```sh
make build              # build all binaries (msc, msc-bench, msc-eval, msc-qa)
make install            # go install ./cmd/msc
go run ./cmd/msc status # quick smoke against a local MuninnDB
```

`msc` needs [MuninnDB](https://github.com/scrypster/muninn) reachable (default
`http://127.0.0.1:8750/mcp`, override with `MUNINN_MCP_URL`).

## Before you open a PR

Everything below must pass — CI enforces it:

```sh
make fmt                # gofmt -w (or: gofmt -l . must be empty)
make lint               # go vet + staticcheck
make test               # go test -race -count=1 ./...
make cover              # coverage report
FUZZTIME=8s make fuzz   # run every fuzz target briefly
```

### Quality bar

- **Every function has a test, and every parsing/transform surface has a fuzz
  target.** New exported behavior ships with both. Fuzz targets assert
  invariants (no-panic is the floor; prefer real properties — idempotence,
  round-trip, UTF-8 validity, bounds).
- **`-race` clean.** Shared state uses `sync`/`sync/atomic`; the store worker is
  single-goroutine by design.
- **gofmt + `go vet` + staticcheck clean.** No new warnings.
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
