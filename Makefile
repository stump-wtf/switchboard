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

test:  ## Run tests
	go test ./...

tidy:  ## Sync go.mod/go.sum
	go mod tidy

ci: vet test build  ## The gate: vet + test + build
