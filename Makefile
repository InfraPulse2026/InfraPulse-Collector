GO ?= go
VERSION ?= dev
COMMIT ?= unknown
BUILD_TIME ?= unknown
LDFLAGS = -X InfraPulseAgent/internal/version.Version=$(VERSION) -X InfraPulseAgent/internal/version.Commit=$(COMMIT) -X InfraPulseAgent/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build check race cross
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/infrapulse-agent ./cmd/infrapulse-agent
	CGO_ENABLED=0 $(GO) build -o bin/infrapulse-mock-controller ./cmd/infrapulse-mock-controller
	CGO_ENABLED=0 $(GO) build -o bin/infrapulse-simulator ./cmd/infrapulse-simulator
	CGO_ENABLED=0 $(GO) build -o bin/infrapulse-mock-enrollment ./cmd/infrapulse-mock-enrollment

check:
	$(GO) fmt ./...
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

cross:
	mkdir -p bin
	@for arch in amd64 arm64 arm mips mipsle; do CGO_ENABLED=0 GOOS=linux GOARCH=$$arch GOARM=7 GOMIPS=hardfloat $(GO) build -ldflags '$(LDFLAGS)' -o bin/infrapulse-agent-linux-$$arch ./cmd/infrapulse-agent || exit 1; done
	@for arch in amd64 arm64; do for binary in infrapulse-mock-controller infrapulse-simulator infrapulse-mock-enrollment; do CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -o bin/$$binary-linux-$$arch ./cmd/$$binary || exit 1; done; done
