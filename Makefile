# switchboard — local dev entry points. `make ci` runs the gate you can reproduce before a PR.
.PHONY: build run fmt vet lint test tidy ci

build:  ## Compile the switchboard binary (assets embedded)
	go build -o bin/switchboard ./cmd/switchboard

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
