# AGENTS.md

Instructions for automated coding agents working in this repository.

## Purpose

This Go ACP agent wraps the local Claude Code CLI using
`github.com/coder/acp-go-sdk`. Session-scoped CLI processes own model execution;
the adapter owns ACP dispatch, process boundaries, mapping, and durable state.

## Project Map

- `cmd/acp-go-claude`: ACP stdio entrypoint, tracing, and signal handling.
- Root `agent*.go`, `options.go`, `request_builders.go`: public API and dispatch.
- `session*.go`, `raw_events.go`: prompt/lifecycle ownership, callbacks,
  cancellation, transcript replay, and session storage.
- `claude_*config.go`, `claude_environment.go`, `claude_settings_files.go`:
  effective native environment, provider/model resolution, and settings.
- `auth*.go`, `host_authority*.go`, `image*.go`: provider auth, borrowed
  process/tree authority, and bounded media/handoff/artifact handling.
- `internal/claude`: native command construction, process control, stream-json,
  and control protocol. `internal/mapper`: ACP/native mapping.
- `internal/permissions`, `internal/transcript`: permission persistence and
  transcript discovery/replay.
- `internal/lifecycle`, `testdata/lifecycle`: negotiation, decoding, reduction,
  emission, and canonical vectors. `internal/observer`: instrumentation.
- `integration`: explicitly gated native tests and deterministic MCP helpers.
- `docs/`, `docs.json`, `examples/`: public guidance and embedding examples.

## Commands

- `make test`: race-enabled, shuffled unit suite with the configured timeout.
- `make lint`: pinned golangci-lint; rules live in `.golangci.yml`.
- `make coverage-check`: the full race/shuffle suite with coverage reporting.
- `make docs-audit`: required documentation and CLI registration checks.
- `make audit`: full local gate, including module tidy. Inspect the target and
  select checks appropriate to the task.

Native tests require explicit task authorization. Use the existing
`test-integration-smoke`, `test-integration-native-browser`,
`test-integration-live`, `test-integration-attended`, and
`test-integration-keystore` targets for their respective proofs; none is part
of `make audit`. The targets set execution gates, which do not themselves
authorize model spend, account interaction, or browser execution.
`make test-integration-cover` collects compiled-adapter coverage.

## Coding Rules

- Follow standard Go idioms: `ctx` first, no context stored in structs, and
  `%w` for wrapped errors.
- Keep the public API small and ACP-oriented; native protocol and process
  details belong in `internal/claude`. Follow existing domain structure,
  structured protocol types, and JSON decoding.
- Preserve validation, error identities, state ownership, and current public
  behavior. Update local docs for API, CLI, metadata, or storage changes.
- Preserve native cleanup and owed durable commits before reporting the
  corresponding completion. Advertise lifecycle facts only when the configured
  boundary proves them; whole-tree quiescence requires authority evidence.
  Follow the scoped prompt/close ordering in
  [session behavior](docs/core/sessions.mdx) and
  [managed execution](docs/operations/security.mdx).
- Keep instructions and public documentation self-contained and current.

## Testing Rules

- Use `testify/require` and focused regressions for changed behavior. Prefer
  table-driven protocol/mapping cases.
- Use `make test` for session, MCP, concurrency, and cancellation changes;
  run the pinned `make lint` before completion.
- Review coverage for meaningful behavioral gaps; do not add padding to reach a
  percentage. Preserve canonical lifecycle fixture bytes and reduce emitted
  notifications through the same reducer used by the vectors.
- Unit tests may use in-memory transports. Native compatibility evidence uses
  the actual CLI; deterministic MCP servers/proxies do not establish native
  behavior.
- Keep live prompts deterministic and assert streamed updates and stop reason.
  Use editing tools such as `Write` when a test needs a permission request;
  user settings may allow `Bash` without prompting.
- Native tests use an isolated temporary `CLAUDE_CONFIG_DIR`.
  `ACP_GO_CLAUDE_HOME` selects the source config copied there;
  `ACP_GO_CLAUDE_MODEL` overrides the live model. Without an explicit source
  home, environment auth permits a fresh home. Missing environment or portable
  file auth must fail without falling back to the operator's normal home.

## Security And Boundaries

- Honor authorization already provided by the task. Ask only before a material
  scope expansion, including unrequested public extensions, permission-flow
  changes, MCP token-policy changes, or session-store contract changes.
- Keep stdout exclusively for ACP JSON-RPC; diagnostics belong on stderr.
  Never log auth material, secrets, prompts, tool payloads, or raw native events
  by default. Preserve session-scoped permission rules and intentional fork copies.
- Every managed native launch uses the supplied `HostAuthority`, without
  ordinary fallback. Prepared trees remain inaccessible until successful
  reclaim; failed preparation leaves cleanup with the host.
- ACP `logout` clears adapter-owned session state. Native account changes
  belong to explicitly selected provider-auth operations. Native-login
  disconnect requires exact resolved-home consent; secret-binding disconnect
  is ledger-only.
- Preserve permission prompts and explicit unsupported-method errors. Keep
  filesystem/network test effects limited to the boundary under test.
