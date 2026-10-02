VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint check e2e e2e-codex e2e-copilot e2e-gemini install dist image

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

# dist builds the release archives into dist/, as the release workflow does.
dist:
	./scripts/release.sh $(VERSION)

# image builds the server's container image.
image:
	docker build --build-arg VERSION=$(VERSION) -t intagent:$(VERSION) .

# check is what CI runs.
check: lint race

# e2e runs two real Claude Code agents against a local server (needs an authenticated claude CLI).
e2e:
	./scripts/e2e-claude.sh

# e2e-codex runs the real Codex CLI against a scripted model: offline and free (needs npm and jq).
e2e-codex:
	./scripts/e2e-codex.sh

# e2e-copilot runs the real GitHub Copilot CLI against a scripted model: offline and free (needs npm).
e2e-copilot:
	./scripts/e2e-copilot.sh

# e2e-gemini runs the real Gemini CLI against a scripted model: offline and free (needs npm).
e2e-gemini:
	./scripts/e2e-gemini.sh
