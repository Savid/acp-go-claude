# acp-go-claude

Go ACP agent for Claude Code. It wraps the local `claude` CLI, speaks
Agent Client Protocol over JSON-RPC streams, and is built on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

Use it as either:

- a standalone ACP subprocess: `acp-go-claude`
- an embedded Go adapter through `claudeacp.Serve`

## Install

```sh
go install github.com/savid/acp-go-claude/cmd/acp-go-claude@latest
```

For local development:

```sh
go run ./cmd/acp-go-claude
```

The process speaks ACP over stdin/stdout. In normal use, an editor or ACP host
launches it as a subprocess rather than a human-facing chat UI.

## Quickstart

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with hello from ACP"
```

Or try the interactive example:

```sh
go run ./examples/interactive-chat
```

## Embedded Go

```go
package main

import (
 "context"
 "log"
 "os"

 claudeacp "github.com/savid/acp-go-claude"
)

func main() {
 err := claudeacp.Serve(context.Background(), os.Stdin, os.Stdout,
  claudeacp.WithDefaultModel("sonnet"),
 )
 if err != nil {
  log.Fatal(err)
 }
}
```

See [Go API docs](docs/reference/go-api.mdx) for options such as Claude path,
Claude home, default model, session storage, permissions, raw events, and
OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  extension-based fork.
- Claude stream-json subprocess management and control protocol handling.
- Prompt streaming for messages, thoughts, tool calls, tool results, plans,
  usage, and session metadata.
- Claude Code structured output through session-level JSON Schema.
- Claude permission modes, permission prompts, plan mode, elicitation, and
  `AskUserQuestion` bridging.
- MCP stdio and HTTP server declarations.
- Optional durable mirroring through a host-provided `SessionStore`.
- Optional raw Claude stream-json extension notifications.
- OpenTelemetry spans, metrics, trace propagation, and structured logs without
  recording prompt/tool secrets by default.
