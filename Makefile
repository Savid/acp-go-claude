.DEFAULT_GOAL := help
.PHONY: lint lint-gopls fmt test coverage-check test-cross-compile test-integration test-integration-cover clean tidy vuln modernize-check audit test/cover help

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## lint-gopls: show gopls hint-level diagnostics (advisory, not enforced)
lint-gopls:
	gopls check -severity=hint $$(find . -name '*.go' -not -path './examples/*') 2>&1 | sed "s#$(CURDIR)/##" | grep -v '\[windows\]' || true

## fmt: format code
fmt:
	golangci-lint fmt ./...

## test: run tests with race detector
test:
	go test -race -shuffle=on -coverprofile=coverage.out -covermode=atomic ./...

## coverage-check: require 100% statement coverage
coverage-check: test
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0 } /^total:/ { found = 1; if ($$3 != "100.0%") { printf "total coverage %s, want 100.0%%\n", $$3; exit 1 } } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-cross-compile: compile platform-specific test branches
test-cross-compile:
	rm -rf .tmp/cross
	mkdir -p .tmp/cross
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/permissions-windows.test ./internal/permissions

## test-integration: run integration tests
test-integration:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 go test -race -tags=integration -timeout=600s -parallel=4 -v ./integration/...

## test-integration-cover: run integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-claude ./cmd/acp-go-claude
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-claude GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -tags=integration -timeout=600s -parallel=4 -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out

## tidy: tidy go modules
tidy:
	go mod tidy

## vuln: run govulncheck
vuln:
	go tool govulncheck ./...

## modernize-check: preview Go modernizations without changing files
modernize-check:
	go fix -n ./...

## audit: run all checks
audit: lint coverage-check test-cross-compile vuln modernize-check
	go mod tidy -diff
	git diff --exit-code -- go.mod go.sum
	go mod verify

## test/cover: open HTML coverage report
test/cover: test
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
