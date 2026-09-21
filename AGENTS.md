# AGENTS.md

## Purpose

This Go module exposes the local `claude` CLI as an Agent Client Protocol agent.
Each ACP session drives one Claude stream-json process that inherits the
adapter's environment and keeps its session in claude's own home, so a session
started over ACP can be continued natively with `claude --resume` afterwards.

## Project Map

- `cmd/acp-go-claude`: stdio entrypoint, OpenTelemetry setup, signals, flags.
- Root `agent*.go`, `options.go`, `request_builders.go`: the public ACP
  surface, option validation, and the account-usage read; the shared ACP
  transport orders publication.
- Root `session*.go`, `image_output.go`: one session's process, event pump,
  prompt turns, permissions and elicitation, lifecycle stream, store mirror,
  replay, config options, and image output.
- `internal/claude`: the stream-json and control client, native message types,
  the account-usage read, launch arguments, and transcript paths.
- `integration`: gated tests against the installed claude.

## Commands

```sh
make build
make test
make lint
make audit
make test-integration-smoke
make test-integration-live
```

`make test` runs with race detection and shuffled order. `make audit` is the
full local gate. Integration targets need an installed `claude`; the live target
spends model tokens and requires explicit operator intent.

## Coding Rules

- Follow Go idioms: `ctx` first, `%w` for wrapped errors, small interfaces at
  the consumer. Keep native protocol details in `internal/claude` and ACP glue
  beside its handler.
- Shared family behavior comes from `github.com/savid/acp-go-core`; never copy
  it here.
- The adapter does no isolation: claude inherits the process environment, the
  agent overlay, then the session env, and drops only its own
  `ACP_GO_CLAUDE_INTERNAL_*` markers.
- Native state is never deleted. The session store is the durability
  boundary; claude's own session file is the native copy.
- Unit tests never require an installed claude: the test binary doubles as a
  scripted fake claude. Keep the fake's protocol in step with `internal/claude`.
- A comment states what the code does or why a constraint exists.

## Verification

Run `go test ./...` for ordinary changes and `make lint` for Go edits. Run
`make audit` once changes settle. Run the integration smoke target after
changing anything claude-facing.

## Boundaries

- Native permission policy decides when a callback is needed. Relay that
  callback through ACP and deny on a failed or cancelled answer.
- Do not log prompts, tool input or output, or raw native event bodies by
  default.
- Serve `_claude/accountUsage` and reject every other ACP extension method;
  the only outbound extension surface is the raw-event notification.
