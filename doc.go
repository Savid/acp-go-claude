// Package claudeacp exposes the local Claude Code CLI as an Agent Client
// Protocol agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve starts Claude sessions on demand, maps ACP requests into Claude CLI
// stream-json/control-protocol messages, and streams ACP session updates back
// to the client. Hosts must complete ACP initialization before issuing session
// or other agent methods.
//
// Hosts should use [Serve] for the JSON-RPC transport. Claude authentication
// and account configuration remain owned by the local Claude Code installation.
//
// Hosts that need durable remote resume can provide [WithSessionStore]. A
// session store receives Claude transcript mirror rows, can back session/list,
// and can hydrate Claude JSONL into a temporary Claude config directory for
// session/load or session/resume when the local Claude transcript is absent.
// Claude-specific session import extension methods are advertised under
// _meta.claude when the agent is initialized.
//
// Hosts that need Claude-specific goal UI can attach initial goal metadata with
// [WithSessionGoal] or build _claude/session/setGoal extension params with
// [SetGoalRequest] and [ClearGoalRequest]. Goals are exposed as
// _meta.claude.goal and are not a portable ACP core field.
//
// Hosts that need adapter telemetry can provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]. The package never configures
// global OpenTelemetry providers; the acp-go-claude binary handles env-based
// exporter setup for command-line use. Caller-supplied providers remain owned
// by the caller, including ForceFlush and Shutdown.
//
// Hosts that need deterministic Claude sessions can use [WithBareMode] or
// per-session [ClaudeOptions] to launch Claude with --bare.
package claudeacp
