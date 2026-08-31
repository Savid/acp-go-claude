.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: build lint fmt-check fmt test test-cross-compile test-integration-smoke test-integration-live tidy vuln audit clean help

build:
	go build ./...

lint:
	$(GOLANGCI_LINT) run ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
	$(GOLANGCI_LINT) fmt ./...

test:
	go test -race -shuffle=on -timeout=20m ./...

test-cross-compile:
	GOOS=linux GOARCH=amd64 go build ./...
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=freebsd GOARCH=amd64 go build ./...
	GOOS=openbsd GOARCH=amd64 go build ./...

test-integration-smoke:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=300s -parallel=4 -v ./integration/...

test-integration-live:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/...

tidy:
	go mod tidy -diff

vuln:
	go tool govulncheck ./...

audit: fmt-check lint build test test-cross-compile tidy vuln
	go mod verify

clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

help:
	@printf '%s\n' 'build lint fmt-check fmt test test-cross-compile test-integration-smoke test-integration-live tidy vuln audit clean'
