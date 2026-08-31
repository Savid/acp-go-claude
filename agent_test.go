package claudeacp

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentCloseIsConcurrentAndKeepsAuthorityFailure(t *testing.T) {
	agent := NewAgent()
	agent.recordContainmentError(ErrContainmentIncomplete)

	results := make(chan error, 8)
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			results <- agent.Close()
		}()
	}
	callers.Wait()
	close(results)

	for err := range results {
		require.ErrorIs(t, err, ErrContainmentIncomplete)
	}
	require.ErrorIs(t, agent.Close(), ErrContainmentIncomplete)
}
