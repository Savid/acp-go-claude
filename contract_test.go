package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestProtocolAdmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	init := h.initialize(withLifecycle())
	require.Empty(t, init.AuthMethods)
	_, err := h.conn.Authenticate(h.ctx(), acp.AuthenticateRequest{MethodId: "native-login"})
	require.Equal(t, -32602, requestErrorCode(t, err))
	require.Equal(t, "native-login", requestErrorData(t, err)["methodId"])
	answer, ok := init.Meta[wire.LifecycleKey].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 1, answer["version"])
	require.Equal(t, true, answer["updatesOutsidePrompt"])
	_, err = h.conn.NewSession(h.ctx(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{{Stdio: new(acp.McpServerStdio)}}})
	require.Equal(t, "mcpServers", requestErrorData(t, err)["field"])
}
func TestNegativeClientCallLimitReturnsOptionsError(t *testing.T) {
	t.Parallel()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: -1}))
	defer agent.Close()
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.Equal(t, "claude_invalid_options", requestErrorData(t, err)["error"])
}
