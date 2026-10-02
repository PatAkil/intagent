VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint check e2e install

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o intagent ./cmd/intagent

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/intagent

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run ./...

# check is what CI runs.
check: lint race

# e2e runs two real Claude Code agents against a local server (needs an authenticated claude CLI).
e2e:
	./scripts/e2e-claude.sh
