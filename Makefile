VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build install test lint clean

build:
	go build -ldflags '$(LDFLAGS)' -o msc ./cmd/msc/

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/msc/

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -f msc
