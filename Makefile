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

.PHONY: help doctor tools tools-staticcheck tools-govulncheck check build build-all build-matrix install \
	test test-short test-fast cover lint lint-go lint-available check-race check-release vet vuln fmt \
	fmt-check tidy tidy-check clean eval eval-models fuzz bench versions

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

# The non-Go linters CI runs through pipx, pinned per run. A pin that lives only
# in ci.yml is one no local command can name, and an unpinned local copy is how
# a green `make check` turns into a red push: new rules land in every ruff
# release. `make versions` prints these in a form ci.yml evals, so the pin is
# stated once and `make lint` can report the drift it is about to run against.
RUFF_VERSION ?= 0.16.4
YAMLLINT_VERSION ?= 1.38.0

versions:
	@echo 'RUFF_VERSION=$(RUFF_VERSION)'
	@echo 'YAMLLINT_VERSION=$(YAMLLINT_VERSION)'

help:
	@echo 'dev targets:'
	@echo '  make doctor       check the toolchain against what this Makefile needs, before anything else'
	@echo '  make tools        go install the two CI linters (staticcheck, govulncheck) into GOBIN'
	@echo '  make tools-staticcheck   just the staticcheck install (what the CI test job runs)'
	@echo '  make tools-govulncheck   just the govulncheck install (what the CI vuln job runs)'
	@echo '  make check        everything CI runs locally: tidy-check fmt-check lint-available lint test build-all check-release'
	@echo '  make check-release  the changelog matches the tag: sections in version order, every version linked, (with TAG=vX.Y.Z) the tagged version documented'
	@echo '  make test         go test -race -count=1 $(PKG)   (override PKG=... or RUN='"'"'^TestFoo$$'"'"')'
	@echo '  make test-short   test with -short: the few wall-clock-dependent tests skip, ~half the run'
	@echo '  make test-fast    same without -race, for a quicker loop'
	@echo '  make fmt          gofmt -w over the tree'
	@echo '  make fmt-check    fail on unformatted files (what CI does)'
	@echo '  make lint         go vet + staticcheck (both required, like CI) + shellcheck, ruff, yamllint where installed'
	@echo '  make versions     the ruff/yamllint pins CI runs (eval it to reproduce the CI lint job)'
	@echo '  make build-matrix compile for every GOOS/GOARCH the CI build job covers'
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

# First-run preflight. Everything below assumes a Go new enough for the go.mod
# directive and, for the race targets, a working C compiler. Both are silent
# failures otherwise: a Go too old reports "note: module requires Go 1.25" as the
# last line of an unrelated build error, and a missing compiler reports only
# "-race requires cgo", which names the wrong knob (the fix is a compiler, not
# an env var). Run this before `make check` when a command fails oddly.
# The Go and C-compiler lines decide the exit status: a preflight that prints
# "you need a newer Go" and returns 0 is a check nobody can gate on. The linter
# lines do too. They used to stay advisory, on the theory that `make check`
# already fails hard on them through lint-available and lint-go, but that made
# the preflight call a bare machine ready and then let `make check` stop on its
# first lint step: a doctor that prints "done" on a clean clone is the failure
# it exists to prevent. A missing linter is a missing prerequisite, and
# prerequisites are what this target reports.
doctor:
	@required=$$(awk '/^go /{print $$2}' go.mod); \
	 have=$$(go env GOVERSION); have=$${have#go}; status=0; \
	 if awk -v req="$$required" -v have="$$have" 'BEGIN{ \
	       n=split(req,r,"."); m=split(have,h,"."); \
	       for (i=1; i<=n; i++) { hv=(i<=m?h[i]:0)+0; if (hv != r[i]+0) exit (hv>r[i]+0 ? 0 : 1) } \
	       exit 0 }'; then \
	   echo "go:      ok ($$have, go.mod requires $$required)"; \
	 else \
	   echo "go:      go$$have installed, go.mod requires $$required or newer" >&2; \
	   status=1; \
	 fi; \
	 cgo=$$(go env CGO_ENABLED); cc=$$(go env CC); \
	 if [ "$$cgo" != "1" ]; then \
	   echo "cc:      CGO_ENABLED=$$cgo; 'make test' needs -race, which needs cgo ('make test-fast' does not)" >&2; \
	 elif command -v "$$cc" >/dev/null 2>&1; then \
	   echo "cc:      ok ($$cc, for -race)"; \
	 else \
	   echo "cc:      '$$cc' not on PATH; 'make test' needs it for -race ('make test-fast' does not)" >&2; \
	   status=1; \
	 fi; \
	 for t in staticcheck govulncheck; do \
	   command -v $$t >/dev/null 2>&1 || { \
	     echo "$$t: missing; 'make check' and CI run it — 'make tools' installs it" >&2; status=1; }; \
	 done; \
	 for t in shellcheck ruff yamllint; do \
	   command -v $$t >/dev/null 2>&1 || { \
	     echo "$$t: missing; 'make check' and CI run it — pipx install $$t (shellcheck comes from your package manager)" >&2; \
	     status=1; }; \
	 done; \
	 if [ "$$status" -eq 0 ]; then echo "doctor: done"; \
	 else echo "doctor: prerequisites above are missing; 'make build' and 'make test-fast' need only go and a C compiler" >&2; fi; \
	 exit $$status

# The `-race` half of the preflight, run on its own by the targets that need
# it so the failure arrives before the compile rather than inside it.
check-race:
	@if [ "$$(go env CGO_ENABLED)" != "1" ]; then \
	  echo "CGO_ENABLED=$$(go env CGO_ENABLED): go test -race needs cgo" >&2; \
	  echo "run 'make test-fast' (no -race), or set CGO_ENABLED=1 for this run" >&2; \
	  exit 1; \
	fi; \
	cc=$$(go env CC); \
	command -v "$$cc" >/dev/null 2>&1 || { \
	  echo "no C compiler '$$cc' on PATH: go test -race links the race runtime through cgo" >&2; \
	  echo "Debian/Ubuntu: apt install build-essential   macOS: xcode-select --install" >&2; \
	  echo "or run 'make test-fast', which has no -race" >&2; \
	  exit 1; \
	}

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
# pushing: anything it misses is a red CI run. lint-available is in the list
# because CI installs the non-Go linters and fails without them, so a local
# `make check` that skipped one would report green and turn red after the push.
check: tidy-check fmt-check lint-available lint test build-all check-release

# CI runs `go mod tidy` and fails if it changes anything, so a stale go.mod
# only surfaces after a push. Same check, same message, locally: the workflow
# calls this target rather than keeping a second copy of the logic. Under
# GitHub Actions each failure also emits an annotation, which lands on the run
# summary instead of scrolling past in the step log.
tidy-check:
	@go mod tidy || { echo "go mod tidy failed; go.mod/go.sum are unverified" >&2; \
	  if [ "$$GITHUB_ACTIONS" = "true" ]; then \
	    echo "::error::go mod tidy failed; go.mod/go.sum are unverified"; \
	  fi; \
	  exit 1; }
	@if [ -n "$$(git status --porcelain go.mod go.sum)" ]; then \
	  echo "go mod tidy changed go.mod/go.sum — commit the result" >&2; \
	  if [ "$$GITHUB_ACTIONS" = "true" ]; then \
	    echo "::error::go mod tidy changed go.mod/go.sum — commit the result"; \
	  fi; \
	  git status --porcelain go.mod go.sum; \
	  git diff go.mod; \
	  exit 1; \
	fi

# The release contract. The tag is the only place the version number lives
# (build stamps it from `git describe`), and a release published without the
# matching notes cannot be corrected afterwards, so the tag and the changelog
# are checked against each other before the tag is pushed. With no TAG, HEAD
# is used if it carries a tag; otherwise only the file-level invariants run
# (section order, every version linked, `[Unreleased]` present), which is why
# `check` can call this on any commit.
check-release:
	bash scripts/check-release-notes.sh $(TAG)

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

# Every GOOS/GOARCH the CI build job covers, in the same order. A syscall that
# only exists on one platform is the usual cross-compile breakage, and the
# matrix is the only job that catches it before release; this is the same check
# without a push. Cross-compiling needs no C toolchain: CGO_ENABLED=0 below.
build-matrix:
	@set -e; for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
	  echo "== $$target =="; \
	  CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} make build-all vet; \
	done

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

test: check-race
	go test -race -count=1 $(if $(RUN),-run '$(RUN)') $(PKG)

# The three store tests that spend real seconds waiting out a retry budget or a
# drain deadline guard themselves with testing.Short, so -short drops the tree
# from ~30s to ~15s. It skips those, and only those: every other test still
# runs, under -race, exactly as `make test` runs it.
test-short: check-race
	go test -short -race -count=1 $(if $(RUN),-run '$(RUN)') $(PKG)

# Same packages without -race: much quicker while iterating, still worth a
# race-enabled `make test` before pushing.
test-fast:
	go test -count=1 $(if $(RUN),-run '$(RUN)') $(PKG)

cover: check-race
	go test -race -count=1 -coverprofile=cover.out ./...
	go tool cover -func=cover.out

# Run all benchmarks with allocation stats (no tests, no fuzzing).
bench:
	go test -run='^$$' -bench=. -benchmem ./...

# Run every fuzz target in the tree for a few seconds each (smoke regression of
# all parsing/transform surfaces). FUZZTIME overrides the per-target budget.
# `go test -list` keeps its stderr and its exit status. A package that fails to
# compile, or a `go test` that errors, would otherwise print nothing on stdout,
# match no target, and leave the campaign reporting "all fuzz targets clean"
# with that package's targets silently uncovered. A package that simply has no
# fuzz target still exits 0, so that case stays a no-op rather than a failure.
FUZZTIME ?= 5s
# A package whose listing fails aborts the run (set -e on the assignment), and a
# campaign that found no target at all fails at the end: a silently empty list
# would turn the CI fuzz job green without fuzzing anything.
fuzz:
	@set -e; ran=0; for pkg in $$(go list ./...); do \
	  targets=$$(go test -list '^Fuzz' $$pkg) || \
	    { echo "go test -list failed for $$pkg" >&2; exit 1; }; \
	  for fn in $$(printf '%s\n' "$$targets" | grep '^Fuzz' || true); do \
	    ran=$$((ran + 1)); \
	    echo "== $$pkg $$fn =="; \
	    go test $$pkg -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) && continue; \
	    echo "   retrying $$fn once (a real crasher is saved to testdata and re-fails; this only absorbs loaded-runner 'context deadline exceeded' flakes)"; \
	    go test $$pkg -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) || exit 1; \
	  done; \
	done; \
	if [ "$$ran" -eq 0 ]; then \
	  echo "no fuzz targets found: 'go test -list ^Fuzz' matched nothing in any package" >&2; \
	  exit 1; \
	fi; \
	echo "all $$ran fuzz targets clean"

# The non-Go linters are required, for the same reason staticcheck is: a
# missing copy that skips silently reports a green `make check` and turns into a
# red CI run. Gate presence with `command -v` and fail in a separate statement:
# `command -v X && X ... || echo skip` also fires the skip on the tool's own
# non-zero exit, so a real finding would print "not installed" and the target
# would still succeed. CI runs the same three, so the local and remote rule sets
# are the same set. Their version pins are RUFF_VERSION/YAMLLINT_VERSION above:
# CI runs those exactly, while a locally installed copy may be older or newer,
# so each is compared against its pin and any drift is reported rather than
# discovered as a finding after the push.
lint: lint-go
	@command -v shellcheck >/dev/null 2>&1 || { \
	  echo "shellcheck is required (CI runs it): https://www.shellcheck.net/#install" >&2; exit 1; }
	shellcheck test-live.sh scripts/*.sh
	@command -v ruff >/dev/null 2>&1 || { \
	  echo "ruff is required (CI runs it): pipx install ruff" >&2; exit 1; }
	@have=$$(ruff --version | awk '{print $$2}'); \
	 [ "$$have" = "$(RUFF_VERSION)" ] || echo "ruff $$have installed, CI pins $(RUFF_VERSION); a newer copy can report findings CI will not" >&2
	ruff check scripts/
	ruff format --check scripts/
	@command -v yamllint >/dev/null 2>&1 || { \
	  echo "yamllint is required (CI runs it): pipx install yamllint" >&2; exit 1; }
	@have=$$(yamllint --version | awk '{print $$2}'); \
	 [ "$$have" = "$(YAMLLINT_VERSION)" ] || echo "yamllint $$have installed, CI pins $(YAMLLINT_VERSION); a newer copy can report findings CI will not" >&2
	yamllint .

# `make lint` treats the non-Go linters as optional, so a contributor without
# them still gets the Go checks. That leniency is wrong for `make check`, which
# advertises itself as the CI mirror: CI installs all three and fails the run on
# a finding, so a skip here is a green local run and a red push. This gate
# reports the missing tools by name instead of letting the skip pass silently.
lint-available:
	@missing=; \
	for t in shellcheck ruff yamllint; do \
	  command -v $$t >/dev/null 2>&1 || missing="$$missing $$t"; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "CI runs these linters and fails the run without them; not on PATH:$$missing" >&2; \
	  echo "pipx install ruff yamllint   (or pipx run ruff@0.16.4 ... as CI does)" >&2; \
	  echo "shellcheck comes from your package manager (Debian/Ubuntu, brew, dnf)" >&2; \
	  exit 1; \
	fi

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
