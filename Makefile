# switchboard — local dev entry points. `make ci` runs the gate you can reproduce before a PR.
.PHONY: build run fmt vet lint test tidy ci changelog-check

# The build version `switchboard version` reports: the git describe, or VERSION=… on the make line.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:  ## Compile the switchboard binary (assets embedded)
	go build -ldflags "-X main.version=$(VERSION)" -o bin/switchboard ./cmd/switchboard

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

# The CI changelog and upgrade-note checks, run locally against origin/main. The title defaults to
# the last commit subject; pass the PR's with PR_TITLE="feat: …" and its labels with PR_LABELS=a,b.
PR_TITLE ?= $(shell git log -1 --format=%s)
changelog-check:  ## CHANGELOG + upgrade-note rules for this branch (SPEC-0027 REQ-9, REQ-10)
	PR_TITLE="$(PR_TITLE)" PR_LABELS="$(PR_LABELS)" BASE_REF="$(or $(BASE_REF),origin/main)" scripts/check-changelog.sh all
