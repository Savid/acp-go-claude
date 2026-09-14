// Package claudeacp exposes the claude coding agent CLI as an Agent Client Protocol
// agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve starts one Claude stream-json process per ACP session, maps ACP requests
// onto Claude's control protocol, and streams ACP session updates back to the client.
// claude inherits the adapter's environment and keeps its sessions in its own
// home, so a session started over ACP can be continued natively with
// `claude --resume` after the adapter closes.
//
// Hosts that need durable remote resume provide [WithSessionStore]. The
// store mirrors claude's session JSONL rows and backs session/list, session/load,
// and session/resume when the native file is absent.
//
// Hosts that need adapter telemetry provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]; the package never configures
// global providers.
package claudeacp
