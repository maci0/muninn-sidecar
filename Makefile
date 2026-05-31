VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build install test cover lint vuln fmt tidy clean eval eval-models fuzz bench

build:
	go build -ldflags '$(LDFLAGS)' -o msc ./cmd/msc/

eval:
	go run ./cmd/msc-eval -sweep

# Downstream usefulness across local ollama models (needs a seeded vault +
# OpenAI-compatible endpoint). Override MODELS/MODEL_URL/N as needed.
MODELS ?= qwen2.5:1.5b-instruct,gemma2:2b,llama3.2:3b
MODEL_URL ?= http://127.0.0.1:11434/v1
eval-models:
	go run ./cmd/msc-qa -vault msc-squad -model-url $(MODEL_URL) -model "$(MODELS)" -n 20 -min-score 0.1 -max-tokens 256 -md docs/model-eval.md

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/msc/

test:
	go test -race -count=1 ./...

cover:
	go test -race -count=1 -coverprofile=cover.out ./...
	go tool cover -func=cover.out

# Run every fuzz target in the tree for a few seconds each (smoke regression of
# all parsing/transform surfaces). FUZZTIME overrides the per-target budget.
bench:
	go test -run='^$$' -bench=. -benchmem ./...

FUZZTIME ?= 5s
fuzz:
	@set -e; for pkg in $$(go list ./...); do \
	  for fn in $$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz'); do \
	    echo "== $$pkg $$fn =="; \
	    go test $$pkg -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) || exit 1; \
	  done; \
	done; echo "all fuzz targets clean"

lint:
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"

# Scan reachable code against the Go vulnerability DB (CI runs this too).
vuln:
	@command -v govulncheck >/dev/null 2>&1 && govulncheck ./... || echo "govulncheck not installed, skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -f msc testmsc cover.out coverage.html
