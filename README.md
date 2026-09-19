# acp-go-claude

`acp-go-claude` exposes [Claude Code](https://code.claude.com/docs/en/overview)
through the [Agent Client Protocol](https://agentclientprotocol.com).
Each ACP session runs one Claude process using stream-json and the native
control protocol. Claude inherits the adapter's environment and uses its own
settings, authentication, and permission policies.

A conversation started over ACP can continue through the native CLI with the
same home and working directory:

```sh
claude --resume NATIVE_SESSION_ID
```

New, load, and resume responses and session-list entries expose the current
native ID as `_meta.claude.nativeSessionId`. Use it for native CLI continuation.
ACP requests continue to use the stable ACP `sessionId`. The store's configuration
record saves both IDs with the matching native history.

## Install and run

```sh
go install github.com/savid/acp-go-claude/cmd/acp-go-claude@latest
acp-go-claude [-path claude] [-home DIR] [-model MODEL] [-seed-file rel=host]... [-debug]
```

Verified against Claude Code 2.1.273. `-path` selects the executable; `-home`
sets `CLAUDE_CONFIG_DIR`; `-model` selects the default native model identifier.
`-seed-file` writes a file relative to the native home before launch.
`-scratch-dir` selects the parent for temporary quota probes; empty uses system temp. `-version` prints the adapter version. Diagnostics go to
stderr. OpenTelemetry uses the standard `OTEL_*` variables.

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

| Process option | Meaning |
|---|---|
| `WithExecutablePath` | Select the native executable. |
| `WithHome` | Set `CLAUDE_CONFIG_DIR`. |
| `WithEnv` | Overlay the inherited environment. |
| `WithScratchDir` | Parent for temporary quota probes; empty uses system temp. |
| `WithSeedFiles` | Seed native configuration files without overwriting unmanaged files. |
| `WithDefaultModel`, `WithConfiguredModels` | Set the default model and append host-configured model IDs to the native catalog. |
| `WithSessionStore` | Select the durability store. |
| `WithConcurrencyLimits` | Configurable concurrency. |
| `WithImageLimits`, `WithInputHandoffRoot` | Set image byte limits and the root for image handoffs. |
| `WithLogger` | Supply the structured logger. |
| `WithTracerProvider`, `WithMeterProvider`, `WithTextMapPropagator` | Configure OpenTelemetry providers and context propagation. |
| `WithAgentName`, `WithAgentTitle`, `WithAgentVersion` | Set the identity advertised at initialize. |
| `WithClaudeSettingSources` | Select native settings sources. |
| `WithClaudeSettingsFile` | Add a native settings file. |

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

`agentCapabilities._meta.claude.structuredOutput` advertises the schema surface.
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

### Account usage

`_claude/accountUsage` with `{"sessionId": "<id>", "providerId": "anthropic"}` reads the subscription
allowance through that session's process, launching one if needed: the
subscription type as `plan`, the session window as `session`, the weekly
window as `weekly_all`, the fixed weekly windows as `seven_day_oauth_apps`,
`seven_day_opus`, and `seven_day_sonnet`, and each model-scoped weekly window as
`weekly_scoped/<model display name>` with that name as its label, each with
its used percent and reset time. When supplied, explicitly denominated native monetary spending uses its reported
currency and decimal exponent. Initialize advertises the read under
`_meta.claude.accountUsage` as
`{"method": "_claude/accountUsage", "scope": "session", "providers": ["anthropic", "openai-codex", "opencode-go", "openrouter"]}`. The read holds the
session's foreground, so one that arrives during a prompt is refused with
backpressure.
Each limit carries `observedAt`; reading cached data does not renew it.
A native config directory must have its own login to report saved-account usage.
`providerId` is required and selects `anthropic`, `openai-codex`, `opencode-go`,
or `openrouter`. The latter three are read only through the gateway
`ANTHROPIC_BASE_URL` names, when it publishes a usage report; that gateway also
answers `anthropic` when the native report and a setup-token probe supply nothing.
An account that reports allowances but whose report claude could not fetch is
the `account_usage` internal failure, so the host retries rather than records
an account without allowance.

For an effective `CLAUDE_CODE_OAUTH_TOKEN` setup token without native usage,
requesting account usage can spend inference tokens. The adapter forwards at
most one native Haiku request and one native Fable request, with one output
token each and no tools or project context. Input tokens still count.
The requests use a temporary conversation and never change the user session.
Only the first-party HTTPS route and credentials matching native effective
settings are eligible; bare mode, credential helpers, and alternate routes
are ineligible.

Observations are shared across sessions with the same effective credentials.
Haiku refreshes no more than every five minutes and Fable every thirty minutes,
only when a consumer requests usage. Fable must be probed to read its scoped
allowance. An unavailable model is checked again after six hours. Failed
probes back off for five minutes, fifteen minutes, then one hour; provider
retry deadlines also apply. A 429 carrying quota headers is a usable reading.

Native turn quota events update their reported windows. A scoped quota error
invalidates only that window. Repeated errors do not bypass cooldowns; a
reported reset ends exhaustion suppression. Retained readings keep their
original timestamps during failures. An account with no reported windows
answers `{"available": false, "reason": "not_reported"}`.

### Persistence

Provide an `acpcore.SessionStore` through `WithSessionStore` for durable
recovery. The default is an in-memory store. Format
`claude-transcript-jsonl-v1` stores native transcript rows and one current
configuration record under `config`, committed atomically. A session closed
before its first prompt retains its configuration and empty history.

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
