//go:build integration

// Package integration contains live Claude CLI integration coverage.
//
// Run with both the integration build tag and ACP_GO_CLAUDE_RUN_INTEGRATION=1.
// These tests launch the real local claude binary and require an authenticated
// Claude Code installation.
package integration
