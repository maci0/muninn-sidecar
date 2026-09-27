VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# Build date pinned to the source's own commit time (reproducible-builds.org).
# Overridable with SOURCE_DATE_EPOCH, the standard knob packagers set. Falling
# back to wall-clock time would make every build differ from the last.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --pretty=%ct 2>/dev/null || echo 0)
# `date -d @N` is GNU; BSD date (every macOS the release matrix builds for)
# spells it `-r N`, and neither failing must fall through to wall-clock time or
# two builds of one commit disagree. Probe the flag, not the platform.
DATE    ?= $(shell date -u -d "@$(SOURCE_DATE_EPOCH)" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                  || date -u -r "$(SOURCE_DATE_EPOCH)" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                  || echo unknown)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# -trimpath keeps the checkout directory out of the binary, so the same source
# built from /home/u and from /build produces identical output. -buildvcs=false
# drops the automatically embedded VCS stamp, which is redundant with the
# COMMIT/DATE ldflags above and adds a dirty-tree flag on top.
BUILDFLAGS = -trimpath -buildvcs=false

# No package in this module imports "C", so cgo buys nothing and costs two
# things: the build needs a working host C toolchain to be installed at all,
# and the resolver ties the binary to the build host's libc. Pinning it off
# makes the build hermetic and the binary statically linked, so the artifact
# is the same bytes whatever libc happens to sit on the machine. Scoped to the
# build targets, not exported: `go test -race` needs cgo to link the runtime.
CGO = CGO_ENABLED=0

.PHONY: help tools tools-staticcheck tools-govulncheck check build build-all install test test-fast \
	cover lint lint-go vet vuln fmt fmt-check tidy tidy-check clean eval eval-models fuzz bench

# Packages/tests for the `test` target. PKG=./internal/redact narrows the
# edit-test loop to the package being edited; RUN='^TestFoo$' narrows it to one
# test. Both default to everything, so `make test` stays the full race run.
PKG ?= ./...
RUN ?=

# @latest, not a pin: staticcheck v0.5.1 no longer compiles against current Go
# toolchains (stale x/tools), and a linter that does not build is worse than
# one that tracks the toolchain. govulncheck follows Go releases, so the same
# argument applies. Override to reproduce a specific release locally.
STATICCHECK_VERSION ?= latest
GOVULNCHECK_VERSION ?= latest
STATICCHECK_PKG = honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
GOVULNCHECK_PKG = golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

help:
	@echo 'dev targets:'
	@echo '  make tools        go install the two CI linters (staticcheck, govulncheck) into GOBIN'
	@echo '  make tools-staticcheck   just the staticcheck install (what the CI test job runs)'
	@echo '  make tools-govulncheck   just the govulncheck install (what the CI vuln job runs)'
	@echo '  make check        everything CI runs locally: tidy-check fmt-check lint test build-all'
	@echo '  make test         go test -race -count=1 $(PKG)   (override PKG=... or RUN='"'"'^TestFoo$$'"'"')'
	@echo '  make test-fast    same without -race, for a quicker loop'
	@echo '  make fmt          gofmt -w over the tree'
	@echo '  make fmt-check    fail on unformatted files (what CI does)'
	@echo '  make lint         go vet + staticcheck (both required, like CI) + shellcheck, ruff, yamllint where installed'
	@echo '  make lint-go      go vet + staticcheck only, the pair CI runs'
	@echo '  make tidy-check   fail if go mod tidy changes go.mod/go.sum'
	@echo '  make cover        race + coverage report'
	@echo '  make fuzz         brief campaign over every fuzz target (FUZZTIME=60s for longer)'
	@echo '  make vuln         govulncheck against the Go vulnerability DB (govulncheck required)'
	@echo '  make bench        all benchmarks with allocation stats'
	@echo '  make build        build msc, msc-bench, msc-eval, msc-qa'
	@echo '  make build-all    compile every package with the shipped flags, no artifacts left'
	@echo '  make install      go install ./cmd/msc'
	@echo '  make eval         offline selection-quality report + MinScore threshold sweep'
	@echo '  make eval-models  downstream answer quality over local models (needs ollama + a seeded vault)'
	@echo '  make clean        remove built binaries and coverage output'
	@echo 'optional tools:'
	@echo '  go install $(STATICCHECK_PKG)'
	@echo '  go install $(GOVULNCHECK_PKG)'

# `go install` puts the linters in $(go env GOBIN): no sudo, no system packages,
# and the same install CI does. Split per tool so a job that needs one linter
# does not pay to build the other.
tools: tools-staticcheck tools-govulncheck
	@echo "installed into $$(go env GOBIN || echo $$(go env GOPATH)/bin); that directory must be on PATH"

tools-staticcheck:
	go install $(STATICCHECK_PKG)

tools-govulncheck:
	go install $(GOVULNCHECK_PKG)

# The full local mirror of the CI `test` job, in CI's order. Run this before
# pushing: anything it misses is a red CI run.
check: tidy-check fmt-check lint test build-all

# CI runs `go mod tidy` and fails if it changes anything, so a stale go.mod
# only surfaces after a push. Same check, same message, locally: the workflow
# calls this target rather than keeping a second copy of the logic. Under
# GitHub Actions each failure also emits an annotation, which lands on the run
# summary instead of scrolling past in the step log.
tidy-check:
	go mod tidy
	@if [ -n "$$(git status --porcelain go.mod go.sum)" ]; then \
	  echo "go mod tidy changed go.mod/go.sum — commit the result" >&2; \
	  if [ "$$GITHUB_ACTIONS" = "true" ]; then \
	    echo "::error::go mod tidy changed go.mod/go.sum — commit the result"; \
	  fi; \
	  git status --porcelain go.mod go.sum; \
	  git diff go.mod; \
	  exit 1; \
	fi

# Build all binaries. Version ldflags only resolve in cmd/msc (the others have
# no main.version symbol, so -X is a harmless no-op there).
build:
	$(CGO) go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o msc       ./cmd/msc/
	$(CGO) go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o msc-bench ./cmd/msc-bench/
	$(CGO) go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o msc-eval  ./cmd/msc-eval/
	$(CGO) go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o msc-qa    ./cmd/msc-qa/

# Compile every package and command without leaving artifacts behind. This is
# the build CI runs, so the flags above are the ones CI actually checks: a copy
# of the command in ci.yml is a second source of truth that drifts.
build-all:
	$(CGO) go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o /dev/null ./...

eval:
	go run ./cmd/msc-eval -sweep

# Downstream usefulness across local ollama models (needs a seeded vault +
# OpenAI-compatible endpoint). Override MODELS/MODEL_URL/N as needed.
MODELS ?= qwen2.5:1.5b-instruct,gemma2:2b,llama3.2:3b
MODEL_URL ?= http://127.0.0.1:11434/v1
eval-models:
	go run ./cmd/msc-qa -vault msc-squad -model-url $(MODEL_URL) -model "$(MODELS)" -n 20 -min-score 0.1 -max-tokens 256 -md docs/model-eval.md

install:
	$(CGO) go install $(BUILDFLAGS) -ldflags '$(LDFLAGS)' ./cmd/msc/

test:
	go test -race -count=1 $(if $(RUN),-run '$(RUN)') $(PKG)

# Same packages without -race: much quicker while iterating, still worth a
# race-enabled `make test` before pushing.
test-fast:
	go test -count=1 $(if $(RUN),-run '$(RUN)') $(PKG)

cover:
	go test -race -count=1 -coverprofile=cover.out ./...
	go tool cover -func=cover.out

# Run all benchmarks with allocation stats (no tests, no fuzzing).
bench:
	go test -run='^$$' -bench=. -benchmem ./...

# Run every fuzz target in the tree for a few seconds each (smoke regression of
# all parsing/transform surfaces). FUZZTIME overrides the per-target budget.
FUZZTIME ?= 5s
fuzz:
	@set -e; for pkg in $$(go list ./...); do \
	  for fn in $$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz'); do \
	    echo "== $$pkg $$fn =="; \
	    go test $$pkg -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) && continue; \
	    echo "   retrying $$fn once (a real crasher is saved to testdata and re-fails; this only absorbs loaded-runner 'context deadline exceeded' flakes)"; \
	    go test $$pkg -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) || exit 1; \
	  done; \
	done; echo "all fuzz targets clean"

# The non-Go linters are optional, so they skip when missing. Gate presence with
# `if` and run the tool in a separate statement: `command -v X && X ... || echo
# skip` also fires the skip on the tool's non-zero exit, so a real finding would
# print "not installed" and the target would still succeed.
lint: lint-go
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck test-live.sh; else echo "shellcheck not installed, skipping"; fi
	@if command -v ruff >/dev/null 2>&1; then ruff check scripts/ && ruff format --check scripts/; else echo "ruff not installed, skipping"; fi
	@if command -v yamllint >/dev/null 2>&1; then yamllint .; else echo "yamllint not installed, skipping"; fi

vet:
	go vet ./...

# CI runs staticcheck and fails the build, so a missing local copy must fail
# here too: skipping it silently reports green and turns into a red CI run.
# The optional non-Go linters stay out of this target, which is the pair CI runs.
lint-go: vet
	@command -v staticcheck >/dev/null 2>&1 || { \
	  echo "staticcheck is required (CI runs it): go install $(STATICCHECK_PKG)" >&2; exit 1; }
	staticcheck ./...

# Scan reachable code against the Go vulnerability DB (CI runs this too).
vuln:
	@command -v govulncheck >/dev/null 2>&1 || { \
	  echo "govulncheck is required (CI runs it): go install $(GOVULNCHECK_PKG)" >&2; exit 1; }
	govulncheck ./...

fmt:
	gofmt -l -w .
	@if command -v ruff >/dev/null 2>&1; then ruff format scripts/; else echo "ruff not installed, skipping"; fi

# A file gofmt cannot parse makes `gofmt -l` exit non-zero with no stdout, so a
# bare `unformatted="$(gofmt -l .)"` would find nothing and report the tree
# clean. Check the command's status, not just its output.
fmt-check:
	@unformatted="$$(gofmt -l .)" || { \
	  echo "gofmt could not parse the tree (see the syntax error above)" >&2; \
	  if [ "$$GITHUB_ACTIONS" = "true" ]; then \
	    echo "::error::gofmt could not parse the tree"; \
	  fi; \
	  exit 1; \
	}; \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt found unformatted files, run 'make fmt'" >&2; \
	  if [ "$$GITHUB_ACTIONS" = "true" ]; then \
	    echo "::error::gofmt found unformatted files — run 'make fmt'"; \
	  fi; \
	  echo "$$unformatted" >&2; \
	  exit 1; \
	fi

tidy:
	go mod tidy

clean:
	rm -f msc msc-bench msc-eval msc-qa cover.out coverage.html
