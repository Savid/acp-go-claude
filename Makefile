.DEFAULT_GOAL := help

.PHONY: test-trusted-supervisor

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: build lint fmt-check fmt test coverage-check test-cross-compile test-integration-smoke test-integration-live test-integration-attended test-integration-keystore test-integration-cover test-integration-native-browser docs-audit clean tidy vuln modernize-check audit test/cover help

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

## test: run unit tests with race detector and shuffled order
test:
	go test -race -shuffle=on ./...

## test-trusted-supervisor: run Linux root-only native authority tests
test-trusted-supervisor:
	@test "$$(uname -s)" = Linux
	@test "$$(id -u)" -eq 0
	@for directory in /var/lib/acp-go /var/lib/acp-go/agent-identities; do if [ ! -e "$$directory" ] && [ ! -L "$$directory" ]; then install -d -o root -g root -m 0700 "$$directory"; fi; [ "$$(stat -c '%F %u %g %a' -- "$$directory")" = 'directory 0 0 700' ] || { echo "unsafe trusted-supervisor authority directory $$directory" >&2; exit 1; }; done
	@selector='^(Test.*(ProcessIsolationActual|TrustedSupervisor|SupervisorGuardianSIGKILL|SupervisorLivenessSIGKILL|GeneratedNative|BorrowedIdentityAdoption|BorrowedDomainAdoption|BorrowedDisposition|AgentIdentityLock|AgentStandalone|AuthorityDomain|IdentityDisposition|PersistentProof|SupervisorConfigIsSealed|CommandCreatorThread|ProviderCreator|SecurityLimits).*)$$'; listing=$$(mktemp); log=$$(mktemp); rc=$$(mktemp); module=$$(go list -m); status=$$?; \
	[ "$$status" -eq 0 ] || { rm -f "$$listing" "$$log" "$$rc"; exit "$$status"; }; \
	go test -list "$$selector" ./... >"$$listing"; status=$$?; \
	[ "$$status" -eq 0 ] || { rm -f "$$listing" "$$log" "$$rc"; exit "$$status"; }; \
	required='TrustedSupervisor SupervisorGuardianSIGKILL SupervisorGuardianSIGKILLBeforeNativeLaunchRefusesStartAndCompletesAfterECHILD SupervisorLivenessSIGKILL GeneratedNative BorrowedIdentityAdoption BorrowedDomainAdoption BorrowedDisposition AgentIdentityLock AgentStandalone AuthorityDomain IdentityDisposition CommandCreatorThread SecurityLimits ProcessIsolationActual'; case "$$module" in github.com/savid/acp-go-amp|github.com/savid/acp-go-claude|github.com/savid/acp-go-hermes|github.com/savid/acp-go-pi) ;; github.com/savid/acp-go-codex|github.com/savid/acp-go-opencode) required="$$required PersistentProof SupervisorConfigIsSealed ProviderCreator" ;; *) rm -f "$$listing" "$$log" "$$rc"; echo "unrecognized trusted-supervisor module $$module"; exit 1 ;; esac; \
	for class in $$required; do grep -Eq "^Test.*$${class}" "$$listing" || { rm -f "$$listing" "$$log" "$$rc"; echo "trusted-supervisor selector discovered no $${class} tests"; exit 1; }; done; \
	expected=$$(grep -Ec '^Test' "$$listing" || true); rm -f "$$listing"; \
	[ "$$expected" -gt 0 ] || { rm -f "$$log" "$$rc"; echo 'trusted-supervisor selector discovered no tests'; exit 1; }; \
	{ go test -race -count=1 -json -run "$$selector" ./...; echo $$? >"$$rc"; } | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '"Action":"pass","Package":"[^"]+","Test":"Test[^/"]+"' "$$log" || true); skipped=$$(grep -Ec '"Action":"skip","Package":"[^"]+","Test":"Test[^"]+"' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq "$$expected" ] || { echo "trusted-supervisor pass count $$passed, want $$expected"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo "trusted-supervisor skip count $$skipped, want 0"; exit 1; }

## test-integration-native-browser: run current Claude login offline and trace every browser launcher
test-integration-native-browser:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ (set -eu; export ACP_GO_CLAUDE_RUN_INTEGRATION=1; case "$$(uname -m)" in x86_64) goarch=amd64; platform=linux/amd64 ;; arm64|aarch64) goarch=arm64; platform=linux/arm64 ;; *) echo "unsupported native-browser architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	integration/browser_canary/prepare.sh; \
	CGO_ENABLED=0 GOOS=linux GOARCH="$$goarch" go test -c -tags=integration,browsercanary -o .tmp/browser-canary/browser-canary.test .; \
	docker build --platform "$$platform" --tag acp-go-claude-browser-canary --file integration/browser_canary/Dockerfile .; \
	authority_volume=$$(docker volume create); trap 'docker volume rm "$$authority_volume" >/dev/null' EXIT HUP INT TERM; \
	docker run --rm --platform "$$platform" --network none --pid=host --env ACP_GO_CLAUDE_RUN_INTEGRATION=1 --cap-add SYS_PTRACE --security-opt seccomp=unconfined --tmpfs /tmp:rw,exec,size=256m --mount "type=volume,source=$$authority_volume,target=/var/lib/acp-go/agent-identities" acp-go-claude-browser-canary); echo $$? >"$$rc"; } 2>&1 | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '^--- PASS: TestRealNativeBrowserContainment ' "$$log" || true); skipped=$$(grep -Ec '^[[:space:]]*--- SKIP: TestRealNativeBrowserContainment(/| )' "$$log" || true); empty=$$(grep -Ec 'no tests to run' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq 1 ] || { echo "native browser pass count $$passed, want exactly 1"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo 'required native browser canary skipped'; exit 1; }; \
	[ "$$empty" -eq 0 ] || { echo 'required native browser selector ran no tests'; exit 1; }

## coverage-check: require 100% statement coverage with race instrumentation
coverage-check:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@awk 'NR > 1 && $$(NF - 1) > 0 && $$NF == 0 { print "uncovered statement block: " $$0; missed = 1 } END { if (missed) exit 1 }' coverage.out
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0 } /^total:/ { found = 1; if ($$3 != "100.0%") { printf "total coverage %s, want 100.0%%\n", $$3; exit 1 } printf "total coverage %s\n", $$3 } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-cross-compile: compile platform-specific test branches
test-cross-compile:
	rm -rf .tmp/cross
	mkdir -p .tmp/cross
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/permissions-windows.test ./internal/permissions
	GOOS=linux GOARCH=amd64 go test -c -o .tmp/cross/claude-linux.test ./internal/claude
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/claude-darwin.test ./internal/claude
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/claude-cmd-darwin.test ./cmd/acp-go-claude
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/claude-windows.test ./internal/claude
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/claude-cmd-windows.test ./cmd/acp-go-claude
	GOOS=freebsd GOARCH=amd64 go build ./...
	GOOS=openbsd GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=amd64 go build ./...

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=300s -parallel=4 -v ./integration/...

## test-integration-live: run full live integration tests
test-integration-live:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/...

## test-integration-attended: run provider-auth flows a human must complete in real time
# A selector that matches nothing exits zero, so the exit status alone cannot
# tell a completed login from a renamed test. The operator watches this tier for
# a relayed URL, so the output streams through tee rather than being replayed
# after the run, and the log is then read back for a per-test PASS line.
test-integration-attended:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_RUN_ATTENDED=1 go test -race -count=1 -tags=integration -timeout=1200s -v -run TestAttended ./integration/... 2>&1; echo $$? >"$$rc"; } | tee "$$log"; \
	status=$$(cat "$$rc"); ran=$$(grep -c '^--- PASS: TestAttended' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$ran" -gt 0 ] || { echo 'no attended provider-auth login ran: -run TestAttended selected nothing'; exit 1; }

## test-integration-keystore: run credential-residence tests against the container fixture
test-integration-keystore:
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_RUN_KEYSTORE=1 go test -race -count=1 -tags=integration -timeout=900s -v -run TestKeystore ./...

## test-integration-cover: run live integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-claude ./cmd/acp-go-claude
	ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-claude GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -tags=integration -timeout=600s -parallel=4 -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## docs-audit: check public docs, examples, required files, CLI flags, and removed terms
docs-audit:
	@missing=0; for file in README.md doc.go docs.json example_test.go AGENTS.md docs/overview.mdx docs/core/sessions.mdx docs/core/prompt-streaming.mdx docs/features/authentication.mdx docs/features/elicitation.mdx docs/features/mcp.mdx docs/features/models-config.mdx docs/features/permissions.mdx docs/features/raw-events.mdx docs/features/session-store.mdx docs/get-started/examples.mdx docs/get-started/install.mdx docs/get-started/quickstart.mdx docs/get-started/run-modes.mdx docs/operations/observability.mdx docs/operations/security.mdx docs/operations/troubleshooting.mdx docs/reference/acp-methods.mdx docs/reference/cli.mdx docs/reference/go-api.mdx docs/reference/meta.mdx docs/reference/updates.mdx examples/minimal-client/main.go examples/resume-from-file/main.go examples/interactive-chat/main.go; do if [ ! -f "$$file" ]; then echo "missing required docs file: $$file"; missing=1; fi; done; exit $$missing
	@for flag in -path -home -scratch-dir -provider-auth-root -provider-auth-direct-home -model -debug -version; do rg -q -- "$$flag" docs/reference/cli.mdx || { echo "missing CLI flag in docs/reference/cli.mdx: $$flag"; exit 1; }; done
	@for flag in path home scratch-dir provider-auth-root provider-auth-direct-home model debug version; do rg -q -- "\"$$flag\"" cmd/acp-go-claude/main.go || { echo "missing CLI flag registration in main.go: $$flag"; exit 1; }; done
	@! rg -n -- '--cli|claude acp|proxy|compatibility|deprecated|legacy|migration|session/import|sdkMessage|emitRawSDKMessages|setGoal|goals|thoughtLevel|"_meta"\s*:\s*\{[^}]*"mode"|NES|SSE MCP|mcpCapabilities\.acp|\bExportSession\b|\bImportSession\b|\bDeleteSession\b|\bParseConfig\b' README.md doc.go docs.json docs examples cmd/acp-go-claude/*.go AGENTS.md

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## tidy: verify module files are tidy
tidy:
	go mod tidy -diff

## vuln: run govulncheck from the go.mod tool directive
# golang.org/x/vuln v1.4.0 panics in x/tools SSA on Go 1.26 generics;
# keep the tool directive pinned at v1.5.0 or newer.
vuln:
	go tool govulncheck ./...

## modernize-check: preview Go modernizations without changing files
modernize-check:
	go fix -n ./...

## audit: run repository checks
audit: fmt-check lint build test coverage-check test-cross-compile tidy vuln modernize-check docs-audit
	go mod verify

## test/cover: open HTML coverage report
test/cover: coverage-check
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
