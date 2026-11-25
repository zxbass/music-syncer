MODULE   := github.com/zxbass/music-syncer
BINARY   := music-syncer
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/cli.Version=$(VERSION) \
	-X $(MODULE)/internal/cli.Commit=$(COMMIT) \
	-X $(MODULE)/internal/cli.Date=$(DATE)

.PHONY: all build install test test-race vet fmt fmt-check run clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/music-syncer

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/music-syncer

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt required for:"; echo "$$out"; exit 1; fi

run:
	go run ./cmd/music-syncer --help

clean:
	rm -rf bin
