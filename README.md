# acp-go-claude

Go ACP agent that exposes the local Claude Code CLI as an [Agent Client Protocol](https://agentclientprotocol.com/) agent.

[![Go Reference](https://pkg.go.dev/badge/github.com/savid/acp-go-claude.svg)](https://pkg.go.dev/github.com/savid/acp-go-claude)
[![CI](https://github.com/savid/acp-go-claude/actions/workflows/go-test.yml/badge.svg)](https://github.com/savid/acp-go-claude/actions/workflows/go-test.yml)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

It wraps the local `claude` CLI, speaks ACP over JSON-RPC streams, and builds on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

Use it as either:

- a standalone ACP subprocess: `acp-go-claude`
- an embedded Go adapter through `claudeacp.Serve`

## Install

Library:

```sh
go get github.com/savid/acp-go-claude
```

CLI:

```sh
go install github.com/savid/acp-go-claude/cmd/acp-go-claude@latest
```

The `acp-go-claude` binary speaks ACP over stdin/stdout; an editor or ACP host
launches it as a subprocess rather than a human-facing chat UI.

## Quickstart

The example programs run from a checkout of this repo, so clone it first:

```sh
git clone https://github.com/savid/acp-go-claude && cd acp-go-claude
```

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP."
```

Start an interactive session against the agent:

```sh
go run ./examples/interactive-chat
```

Load and resume a stored session transcript:

```sh
go run ./examples/resume-from-file -file ./examples/resume-from-file/session.jsonl
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

See the [Go API reference](https://pkg.go.dev/github.com/savid/acp-go-claude)
for options such as the Claude executable path, config home, scratch directory,
default model, session storage, permissions, raw events, and OpenTelemetry
providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  extension-based fork.
- Negotiated lifecycle streams with prompt/action correlation and close-fenced
  durable settlement.
- Claude stream-json subprocess management and control-protocol handling.
- Prompt streaming for messages, thoughts, tool calls, tool results, plans,
  usage, and session metadata.
- Embedded static PNG, JPEG, GIF, and WebP prompt images, or the same images
  handed over as digest-verified local files under a configured read root, plus
  typed image and resource-link output from native assistant and tool results.
- Structured output through session-level JSON Schema.
- Permission modes, permission prompts, plan mode, elicitation, and
  `AskUserQuestion` bridging.
- MCP stdio and HTTP server declarations.
- Brokered Claude subscription login, setup-token entry, and Anthropic API-key
  entry over session-scoped `_claude/auth/*` extension methods. Secret methods
  use one-shot typed credential harvest and session injection; they never write
  native credential files.
- Store-authoritative transcript mirroring, in-memory by default and replaceable
  with a host-provided `SessionStore` for cross-process durability.
- Optional raw Claude stream-json extension notifications.
- OpenTelemetry spans, metrics, trace propagation, and structured logs without
  recording prompt or tool secrets by default.
- Ordinary same-identity native execution by default on every supported
  platform: native work runs as the adapter's own identity, needs no privileged
  supervisor, and reports non-authoritative `shared_identity` with no
  provider-descendant inventory. Supplying `WithProcessIsolation` selects the
  authoritative Linux boundary instead; that policy is refused before any spawn
  on Windows, Darwin, FreeBSD, and OpenBSD, whose process APIs cannot apply the
  Unix UID/GID identity boundary with empty supplementary groups, and
  cross-compilation proves only that this refusal path builds. Darwin also has
  an explicit best-effort opt-in — mutually exclusive with process isolation —
  for operators who accept escaped-descendant and numeric-PGID-reuse risks.

## Slash Commands

Claude Code slash commands are projected into ACP `AvailableCommand` entries and
refreshed as the session's command set changes. A slash-prefixed prompt runs the
corresponding Claude command.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [Models and config](docs/features/models-config.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Observability](docs/operations/observability.mdx)
- [Security and process containment](docs/operations/security.mdx)

Full Go API reference:
[pkg.go.dev/github.com/savid/acp-go-claude](https://pkg.go.dev/github.com/savid/acp-go-claude).

## Development

```sh
make audit
make test-integration-smoke
make test-integration-live
make test-integration-cover
```

`make audit` runs the full local gate: format, lint, build, unit tests,
coverage, cross-compile, vuln, and docs checks. Live integration tests require a
local authenticated `claude` CLI and are double-gated: the `integration` build
tag plus `ACP_GO_CLAUDE_RUN_INTEGRATION=1`. `make test-integration-smoke` runs
the integration tier without spending model tokens; tests that spend tokens
additionally require `ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1`, which only
`make test-integration-live` sets. `make test-integration-cover` runs the
integration tier against a coverage-instrumented binary. Set `ACP_GO_CLAUDE_MODEL` to override the
live model. Live tests always launch Claude with an isolated temp
`CLAUDE_CONFIG_DIR`; set `ACP_GO_CLAUDE_HOME` to choose the source config copied
into it. When process env auth is present and `ACP_GO_CLAUDE_HOME` is unset,
tests use a fresh temp home; otherwise they copy the source home and clear copied
refresh tokens. If neither env auth nor copied portable file auth is available,
tests fail rather than launch without isolated auth.

## License

Distributed under the GNU General Public License v3.0. See [LICENSE](LICENSE).
