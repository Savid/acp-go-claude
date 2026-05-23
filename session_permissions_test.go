package claudeacp

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionPermissionCacheConcurrentWritesAndReads(t *testing.T) {
	previousSavePermissionRules := savePermissionRules
	savePermissionRules = func(context.Context, string, acp.SessionId, map[string]string) error {
		return nil
	}
	t.Cleanup(func() { savePermissionRules = previousSavePermissionRules })

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	session := &Session{
		agent:           agent,
		id:              "session-1",
		permissionRules: map[string]string{},
	}
	require.NoError(t, agent.storeStartedSession(context.Background(), session))

	const writers = 32
	const readers = 32

	start := make(chan struct{})
	errs := make(chan error, readers)

	var wg sync.WaitGroup
	for index := range writers {
		wg.Go(func() {
			<-start

			behavior := claude.BehaviorAllow
			if index%2 == 1 {
				behavior = claude.BehaviorDeny
			}

			session.setPermissionRule(context.Background(), fmt.Sprintf("Tool%d", index), behavior)
		})
	}

	for range readers {
		wg.Go(func() {
			<-start

			for range writers {
				if _, err := agent.permissionRulesForSession(context.Background(), session.id); err != nil {
					errs <- err

					return
				}
			}
		})
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	rules := session.clonePermissionRules()
	require.Len(t, rules, writers)

	cached, ok := agent.cachedPermissionRules(session.id)
	require.True(t, ok)
	require.Equal(t, rules, cached)
}

func TestSessionSetPermissionRuleDoesNotHoldSessionLockDuringSave(t *testing.T) {
	previousSavePermissionRules := savePermissionRules
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var releaseOnce sync.Once
	savePermissionRules = func(context.Context, string, acp.SessionId, map[string]string) error {
		close(saveStarted)
		<-releaseSave

		return nil
	}
	t.Cleanup(func() {
		savePermissionRules = previousSavePermissionRules
		releaseOnce.Do(func() { close(releaseSave) })
	})

	session := &Session{
		agent:           NewAgent(WithClaudeHome(t.TempDir())),
		id:              "session-1",
		permissionRules: map[string]string{},
	}

	saveDone := make(chan struct{})
	go func() {
		defer close(saveDone)
		session.setPermissionRule(context.Background(), "Read", claude.BehaviorAllow)
	}()

	select {
	case <-saveStarted:
	case <-time.After(time.Second):
		t.Fatal("permission save did not start")
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		behavior, ok := session.permissionRule("Read")
		require.True(t, ok)
		require.Equal(t, claude.BehaviorAllow, behavior)
	}()

	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("session lock was held while permission rules were saved")
	}

	releaseOnce.Do(func() { close(releaseSave) })

	select {
	case <-saveDone:
	case <-time.After(time.Second):
		t.Fatal("permission save did not finish")
	}
}
