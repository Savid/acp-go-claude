package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderNativeOptionsCarryHostAuthority(t *testing.T) {
	authority := newFakeHostAuthority()
	broker, _ := newAuthBroker(t, WithHostAuthority(authority))
	options, err := broker.nativeOptions()
	require.NoError(t, err)
	require.NotNil(t, options.Authority)
	require.Equal(t, broker.home.path, options.ClaudeHome)
}

func TestAuthorityLossStopsNewSessionAdmission(t *testing.T) {
	agent := NewAgent()
	agent.recordContainmentError(ErrHostAuthorityUnavailable)
	require.ErrorIs(t, agent.beginSessionConstruction(), ErrHostAuthorityUnavailable)
}
