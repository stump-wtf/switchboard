# switchboard — local dev entry points. `make check` runs the whole gate you can reproduce before a PR.
.PHONY: build run fmt vet lint tidy-check test tidy ci changelog-check check

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

# tidy-check runs first: an untidy go.sum is what made cairn's v0.2.0 release
# die in goreleaser's dirty-tree check, and this repo's release workflow runs
# `goreleaser release --clean`, which fails the same way if any release step
# rewrites a tracked file. Caught on every PR via `make lint`.
lint: tidy-check  ## golangci-lint (install: https://golangci-lint.run)
	golangci-lint run

# Fail when `go mod tidy` would change go.mod or go.sum, restoring whatever it
# touched so the working tree is left as the caller had it.
tidy-check:
	@d=$$(mktemp -d); cp go.mod go.sum $$d/; \
	go mod tidy || { rm -rf $$d; echo "go mod tidy failed: fix go.mod/go.sum (stale or corrupt checksums?)"; exit 1; }; \
	s=0; cmp -s go.mod $$d/go.mod || s=1; cmp -s go.sum $$d/go.sum || s=1; \
	cp $$d/go.mod $$d/go.sum .; rm -rf $$d; \
	if [ $$s -ne 0 ]; then echo "go mod tidy would change go.mod/go.sum: run 'make tidy' and commit the result"; exit 1; fi

test:  ## Run tests (the sb.js behavioral suite needs node — jsharness_test.go)
	@command -v node >/dev/null 2>&1 || echo "WARNING: node not found in PATH — TestJSModules will SKIP: the sb.js behavioral suite (jstest/) will NOT run. Install Node.js 20+ so the JS quality gate executes." >&2
	go test ./...

tidy:  ## Sync go.mod/go.sum
	go mod tidy

ci: vet test build  ## The gate: vet + test + build

# The CI changelog and upgrade-note checks, run locally against origin/main. The title defaults to
# the last commit subject; pass the PR's with PR_TITLE="feat: …" and its labels with PR_LABELS=a,b.
# The inputs reach the script as exported environment, never pasted into the recipe's shell line, so
# a title carrying quotes or a $ (a Revert "…" subject) is passed through intact.
changelog-check: export PR_TITLE ?= $(shell git log -1 --format=%s)
changelog-check: export PR_LABELS ?=
changelog-check: export BASE_REF ?= origin/main
changelog-check:  ## CHANGELOG + upgrade-note rules for this branch (SPEC-0027 REQ-9, REQ-10)
	scripts/check-changelog.sh all

check: lint ci  ## Everything CI gates: lint + vet + test + build
