// Package integration exercises the installed Claude Code through ACP.
// Smoke runs require ACP_GO_CLAUDE_RUN_INTEGRATION=1; token-spending runs also
// require ACP_GO_CLAUDE_RUN_LIVE_TOKENS=1. ACP_GO_CLAUDE_HOME supplies portable
// credentials copied into a temporary home. Native auth environment variables
// are inherited. ACP_GO_CLAUDE_MODEL selects the model for live tests.
package integration
