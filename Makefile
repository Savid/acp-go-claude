.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: build lint fmt-check fmt test race coverage-check test-cross-compile test-integration-smoke test-integration-live test-integration test-integration-cover docs-audit clean tidy vuln modernize-check audit test/cover help

## build: compile all packages
build:
	go build ./...

## lint: run pinned golangci-lint
lint:
	$(GOLANGCI_LINT) run ./...

## fmt-check: require gofmt-clean Go files
fmt-check:
	@test -z "$$(gofmt -l .)"

## fmt: format Go files
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
	$(GOLANGCI_LINT) fmt ./...

## test: run ordinary unit tests
test:
	go test ./...

## race: run unit tests with the race detector
race:
	go test -race ./...

## coverage-check: write a coverage profile and print totals
coverage-check:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

## test-cross-compile: compile platform-specific test branches
test-cross-compile:
	rm -rf .tmp/cross
	mkdir -p .tmp/cross
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/permissions-windows.test ./internal/permissions

## test-integration-smoke: compile and run integration tests that can skip without live auth
test-integration-smoke:
	go test -tags=integration -timeout=120s -run 'TestNullClaudeRefreshTokens|TestClaudeAccessToken|TestCopyClaudeStateFileUsesSiblingForDefaultHome|TestClaudeSettingsAuthAvailable|TestIsolatedClaudeRuntimeUsesFreshHomeWithProcessAuth|TestIsolatedClaudeRuntimeCopiesExplicitSource' ./integration/...

## test-integration-live: run live Claude CLI integration tests
test-integration-live:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 go test -race -tags=integration -timeout=600s -parallel=4 -v ./integration/...

## test-integration: alias for live integration tests
test-integration: test-integration-live

## test-integration-cover: run live integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-claude ./cmd/acp-go-claude
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-claude GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -tags=integration -timeout=600s -parallel=4 -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## docs-audit: check public docs and examples for removed public terms
docs-audit:
	@! rg -n 'opencode acp|proxy|compatibility|deprecated|legacy|migration|session/import|sdkMessage|emitRawSDKMessages|setGoal|goals|NES|SSE MCP|mcpCapabilities\.acp' README.md doc.go docs.json docs examples cmd/acp-go-claude/*.go

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out

## tidy: verify module files are tidy
tidy:
	go mod tidy -diff

## vuln: run govulncheck from the go.mod tool directive
vuln:
	go tool govulncheck ./...

## modernize-check: preview Go modernizations without changing files
modernize-check:
	go fix -n ./...

## audit: run repository checks
audit: fmt-check lint build test coverage-check test-cross-compile vuln modernize-check docs-audit
	go mod verify

## test/cover: open HTML coverage report
test/cover: coverage-check
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
