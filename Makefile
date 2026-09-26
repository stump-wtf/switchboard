# switchboard — local dev entry points. `make ci` runs the gate you can reproduce before a PR.
.PHONY: build run fmt vet lint test tidy ci

# The build identity every surface reports (internal/buildinfo, SPEC-0027 REQ-1): the git describe,
# the full commit and its RFC 3339 commit date — or VERSION=… / COMMIT=… / DATE=… on the make line.
# An empty value is fine: buildinfo fills it from the binary's embedded VCS info instead.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null)
BUILDINFO := github.com/stump-wtf/switchboard/internal/buildinfo
LDFLAGS := -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

build:  ## Compile the switchboard binary (assets embedded)
	go build -ldflags "$(LDFLAGS)" -o bin/switchboard ./cmd/switchboard

run: build  ## Build and run
	./bin/switchboard

fmt:  ## Format
	gofmt -w .

vet:  ## go vet
	go vet ./...

lint:  ## golangci-lint (install: https://golangci-lint.run)
	golangci-lint run

test:  ## Run tests (the sb.js behavioral suite needs node — jsharness_test.go)
	@command -v node >/dev/null 2>&1 || echo "WARNING: node not found in PATH — TestJSModules will SKIP: the sb.js behavioral suite (jstest/) will NOT run. Install Node.js 20+ so the JS quality gate executes." >&2
	go test ./...

tidy:  ## Sync go.mod/go.sum
	go mod tidy

ci: vet test build  ## The gate: vet + test + build
