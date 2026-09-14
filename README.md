# acp-go-claude

`acp-go-claude` exposes [Claude Code](https://code.claude.com/docs/en/overview)
through the [Agent Client Protocol](https://agentclientprotocol.com).
Each ACP session runs one Claude process using stream-json and the native
control protocol. Claude inherits the adapter's environment and uses its own
settings, authentication, and permission policies.

A conversation started over ACP can continue through the native CLI with the
same home and working directory:

```sh
claude --resume <session-id>
```

## Install and run

```sh
go install github.com/savid/acp-go-claude/cmd/acp-go-claude@latest
acp-go-claude [-path claude] [-home DIR] [-model MODEL] [-seed-file rel=host]... [-debug]
```

Requires Claude Code 2.0.0 or newer. `-path` selects the executable; `-home`
sets `CLAUDE_CONFIG_DIR`; `-model` selects the default native model identifier.
`-seed-file` writes a file relative to the native home before launch.
`-scratch-dir` configures the parent for ephemeral adapter state. `-version`
prints the adapter version. Diagnostics go to stderr. OpenTelemetry uses the
standard `OTEL_*` variables.

## Embed

```go
err := claudeacp.Serve(ctx, os.Stdin, os.Stdout,
    claudeacp.WithHome("/srv/claude"),
    claudeacp.WithSessionStore(store),
)
```

`WithEnv` overlays the inherited environment. Session `env` applies after it;
`WithHome` applies last. Only `ACP_GO_CLAUDE_INTERNAL_*` markers are dropped.
Session `extraPathDirs` prepend to the resulting `PATH`; empty PATH components
are omitted. Executable lookup uses the base environment before session overrides.

Other options configure logging and telemetry, agent identity, model choices,
image limits and input handoff, concurrency, turn deadlines, and store read
timeouts. `WithClaudeSettingSources` selects native settings sources;
`WithClaudeSettingsFile` adds a native settings file;
`WithClaudeInitializeTimeout` bounds the native control handshake.

### Session options

`_meta.claude.options` on new, load, and resume requests, or
`WithSessionClaudeOptions(NewClaudeOptions(...))` from Go:

| Field | Meaning |
|---|---|
| `model` | Native model identifier or alias |
| `env` | Session environment overlay |
| `extraPathDirs` | Ordered absolute directories prepended to PATH |
| `permissionMode` | Native permission policy |
| `systemPrompt` | Native system prompt |
| `bare` | Native bare mode |
| `effort` | Native effort setting |
| `outputSchema` | Nonempty JSON schema for native structured output |

`session/set_config_option` exposes `model`, `mode`, `effort`, and
`output_style` when available. Model and command catalogs come from native
initialization. Structured output appears on usage updates at
`_meta.claude.structuredOutput`. Delegated updates carry
`_meta.claude.parentToolUseId`.

Native permission requests are relayed to ACP; a failed or cancelled answer
denies the operation. Native form and URL elicitations require the matching
client capability. `mcpServers` on ACP requests must be empty.

The lifecycle extension reports session and prompt state. Opting into
`_meta.claude.rawEvent.enabled` also forwards native events on
`_claude/rawEvent`, with inline image payloads redacted.

### Persistence

Provide an `acpcore.SessionStore` through `WithSessionStore` for durable
recovery. The default is an in-memory store. Format
`claude-transcript-jsonl-v1` stores native transcript rows and one current
configuration record under `config`, committed atomically.

Transcripts remain in Claude's home under `projects/<project>/<session-id>.jsonl`.
Load adopts newer native rows when the shared prefix matches, or materializes
missing rows from the store. Divergent logs fail restore. `session/load`
replays history; `session/resume` restores without replay. Close and delete
leave native files in place.

## Development

```sh
make test
make audit
make test-integration-smoke
make test-integration-live
```

Unit tests use a scripted native process. Smoke requires installed Claude and
spends no model tokens. Live tests spend tokens and prove ACP → native CLI →
ACP continuation in a temporary home. Native auth environment variables are
inherited; `ACP_GO_CLAUDE_HOME` optionally supplies a portable credential file.
`ACP_GO_CLAUDE_MODEL` selects the live test model.
