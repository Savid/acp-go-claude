package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionProducerGateOwnsEveryAdmittedProducerThroughClose(t *testing.T) {
	var gate sessionProducerGate
	finish, admitted := gate.begin()
	require.True(t, admitted)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, gate.closeAndWait(canceled), context.Canceled)
	_, admitted = gate.begin()
	require.False(t, admitted)
	finish()
	finish()
	require.NoError(t, gate.closeAndWait(t.Context()))

	var empty sessionProducerGate
	require.NoError(t, empty.closeAndWait(t.Context()))
}

func TestSessionEstablishmentGateFailsClosedAtEveryBoundary(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	require.Error(t, session.installEstablishmentGate(nil))

	previous := uuidRandom
	uuidRandom = bytes.NewReader(nil)
	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	err := session.installEstablishmentGate(client)
	uuidRandom = previous
	require.Error(t, err)

	client = claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, session.installEstablishmentGate(client))
	route := session.establishmentRoute(client)
	require.NotEmpty(t, route)
	require.Empty(t, session.establishmentRoute(claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())))
	require.True(t, session.awaitEstablishmentRoute(t.Context(), "unrelated"))

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, session.awaitEstablishmentRoute(canceled, route))

	session.settleEstablishment(errors.New("publication failed"))
	require.False(t, session.awaitEstablishmentRoute(t.Context(), route))
	_, finish, admitted := session.admitControlCallback(t.Context(), route)
	require.False(t, admitted)
	finish()

	var none *sessionEstablishment
	none.settle(nil)
}
