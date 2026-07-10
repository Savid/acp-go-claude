# AGENTS.md

Shared instructions for automated coding agents working in this repository.

## Purpose

This project is a Go implementation of an ACP agent for Claude Code. It wraps
the local `claude` CLI and builds directly on `github.com/coder/acp-go-sdk`.

## Project Map

Organized by domain. Public surface lives in the root package; implementation
details live in `internal/`.

- **Entrypoint** (`cmd/acp-go-claude`): process entrypoint, ACP stdio mode,
  Claude passthrough mode, tracing setup, and signal handling.
- **ACP agent surface** (root package, e.g. `agent.go`, `agent_connection.go`,
  `agent_dispatcher.go`, `options.go`, `auth.go`, `ids.go`): ACP method
  handlers, request dispatch, agent options, authentication, and ID helpers.
- **Session orchestration** (`session.go`, `session_meta.go`, `raw_events.go`,
  `session_store.go`):
  Claude turn lifecycle, prompts, cancellation, permissions, elicitation,
  usage updates, transcript replay, session storage, and raw event handling.
- **Configuration** (`claude_model_config.go`, `claude_settings_files.go`):
  provider/model resolution and Claude settings-file handling.
- **Claude CLI internals** (`internal/claude`): CLI process management,
  stream-json protocol, control protocol, command construction, event decoding.
- **Protocol mapping** (`internal/mapper`): ACP-to-Claude mapping for prompts,
  MCP config, modes, models, and updates.
- **Session state** (`internal/permissions`, `internal/transcript`): session
  permission persistence and Claude transcript discovery/replay.
- **Live tests** (`integration`): integration tests that launch the real local
  `claude` CLI.
- **Docs** (`docs/`, `docs.json`): Mintlify guide. Update alongside public API,
  CLI flag, ACP method, or `_meta` field changes.

## Commands

```sh
go build ./...
go test ./...
go test -race ./...
golangci-lint run ./...
```

Lint details live in `.golangci.yml`.

The Makefile wraps the main development checks:

```sh
make test
make lint
make audit
```

Run live integration tests only when a local Claude CLI is installed and
authenticated:

```sh
ACP_GO_CLAUDE_RUN_INTEGRATION=1 ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/...
```

`ACP_GO_CLAUDE_RUN_INTEGRATION=1` gates the integration tier;
`ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1` additionally opts in to tests that spend
model tokens (`make test-integration-smoke` omits it).
`make test-integration` runs the same live suite. Use
`make test-integration-cover` for compiled `acp-go-claude` coverage through
`GOCOVERDIR`. Set `ACP_GO_CLAUDE_MODEL` to override the model used by live
tests. Live tests always launch Claude with an isolated temp
`CLAUDE_CONFIG_DIR`. Set `ACP_GO_CLAUDE_HOME` to choose the source Claude config
copied into that temp home. When `ACP_GO_CLAUDE_HOME` is unset and process env
auth is available, tests use a fresh temp home. If neither env auth nor copied
portable file auth is available, tests fail instead of using the normal Claude
home.

## Coding Rules

- Follow standard Go idioms: `ctx` first, no `ctx` in structs, and `%w` for
  wrapped errors.
- Keep the public root package small; implementation details belong in
  `internal/` unless they are part of the public API.
- Prefer structured protocol types and JSON decoding over ad hoc string parsing.
- Preserve ACP method names, request/response shapes, and validation behavior.
- Keep protocol glue narrow, documented, and close to the ACP method it serves.
- Keep shared code next to the domain it serves; avoid generic catch-all
  packages such as `utils`, `helpers`, or `common`.
- Follow existing package patterns before introducing new abstractions.

## Ask Before

Unless explicitly requested, ask before:

- Changing the permission or elicitation flow shape.
- Adding new ACP extension methods or `_meta` fields.
- Modifying MCP bridge token handling.
- Changing the session-store contract.

## Testing Rules

- Use `testify/require` for assertions.
- Prefer table-driven tests for mapper/protocol cases.
- Run `go test ./...` for ordinary changes.
- Run `go test -race ./...` or `make test` for session, MCP, concurrency, or
  cancellation changes.
- Run `golangci-lint run ./...` before considering work complete.
- Live integration tests launch the actual `claude` binary from `PATH`.
- Unit tests may use in-memory transports.
- Local helper processes in integration tests are MCP servers with deterministic
  responses.
- Keep live prompts deterministic with exact sentinel replies, and assert the
  ACP stop reason plus streamed updates where practical.
- Do not rely on `Bash` to trigger permission prompts in live tests; user Claude
  settings may globally allow it. Use editing tools such as `Write` when the
  test needs a permission request.

## Security And Boundaries

- **IMPORTANT**: Do not silently bypass permission prompts. Permission flow is
  load-bearing for user trust in this agent.
- **IMPORTANT**: Do not manage Claude CLI authentication state. ACP `logout`
  only clears adapter-owned session state.
- Do not log auth material, user secrets, prompts, tool input, tool output, or
  raw Claude event bodies by default.
- Keep permission rules session-scoped. Copy them only through intentional
  session fork behavior.
- Reject unsupported ACP extension/provider mutation methods with explicit
  protocol errors unless this agent implements a namespaced extension.
- Avoid broad filesystem or network behavior in tests unless the test is
  explicitly about that boundary.
