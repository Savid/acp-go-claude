package claudeacp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestProviderNativeOptionsCurrentBehavior(t *testing.T) {
	home := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "scratch")
	broker, _ := newAuthBroker(t,
		WithHome(home),
		WithExecutablePath("/bin/claude"),
		WithScratchDir(scratch),
		WithEnv(map[string]string{"A": "B"}),
	)

	options, err := broker.nativeOptions()
	require.NoError(t, err)
	require.Equal(t, "/bin/claude", options.CLIPath)
	require.Equal(t, map[string]string{"A": "B"}, options.Env)
	require.Equal(t, scratch, options.ScratchParent)
	require.Nil(t, options.Authority)

	resolved, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, resolved, options.ClaudeHome)

	occupied := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.WriteFile(occupied, []byte("x"), 0o600))
	broker, _ = newAuthBroker(t, WithHome(t.TempDir()), WithScratchDir(occupied))
	_, err = broker.nativeOptions()
	require.ErrorContains(t, err, "create scratch parent dir")
}

func TestAuthLoginBeginAdapterAndRetainedLoginEdges(t *testing.T) {
	originalStart := authLoginStart
	t.Cleanup(func() { authLoginStart = originalStart })

	authLoginStart = func(context.Context, claude.Options) (*claude.AuthLogin, string, error) {
		return nil, "", errTestRandom
	}
	_, _, err := authLoginBegin(t.Context(), claude.Options{})
	require.ErrorIs(t, err, errTestRandom)

	wantLogin := &claude.AuthLogin{}
	authLoginStart = func(context.Context, claude.Options) (*claude.AuthLogin, string, error) {
		return wantLogin, "https://example.test", nil
	}
	login, authorizeURL, err := authLoginBegin(t.Context(), claude.Options{})
	require.NoError(t, err)
	require.Same(t, wantLogin, login)
	require.Equal(t, "https://example.test", authorizeURL)

	broker := &providerAuth{agent: NewAgent()}
	broker.logAuthLoginGrammar(t.Context(), errTestRandom)
	broker.logAuthLoginGrammar(t.Context(), &claude.AuthLoginGrammarError{
		Line: "open https://example.test/login?code=secret now",
	})

	loginHandle := &authLoginHandle{}
	(*providerAuth)(nil).retainLogin(loginHandle)
	broker.retainLogin(nil)
	broker.retainLogin(loginHandle)
	require.Contains(t, broker.retainedLogins, loginHandle)
	(*providerAuth)(nil).releaseLogin(loginHandle)
	broker.releaseLogin(nil)
	broker.releaseLogin(loginHandle)
	require.NotContains(t, broker.retainedLogins, loginHandle)
	require.NoError(t, (*providerAuth)(nil).retryRetainedLogins())
}

func TestProviderNativeLegsFailClosedOnUnresolvableHome(t *testing.T) {
	newAuthSeams(t)
	broker, _ := newAuthBroker(t, WithHome(filepath.Join(t.TempDir(), "absent")))

	_, cause := broker.readAccount(t.Context())
	require.Equal(t, authCauseProcess, cause)
	requireAuthFailed(t, broker.nativeLogout(t.Context()), authCauseProcess)
	requireAuthFailed(t, broker.removeKeystoreItems(t.Context()), authCauseProcess)
	_, _, cause = broker.startLogin(t.Context())
	require.Equal(t, authCauseProcess, cause)
}

func TestAuthNativeCauseCurrentClassification(t *testing.T) {
	broker, _ := newAuthBroker(t)

	require.Equal(t, authCauseProcess, broker.authNativeCause(ErrContainmentIncomplete))
	require.ErrorIs(t, broker.agent.containmentErr, ErrContainmentIncomplete)
	require.Equal(t, authCauseTimeout, broker.authNativeCause(context.DeadlineExceeded))
	require.Equal(t, authCauseProcess, broker.authNativeCause(errTestRandom))
}

func TestReadAccountCurrentBehavior(t *testing.T) {
	seams := newAuthSeams(t)
	broker, _ := newAuthBroker(t)
	seams.account = claude.AuthAccount{LoggedIn: true, AuthMethod: "oauth", Email: "owner@example.test"}

	reading, cause := broker.readAccount(t.Context())
	require.Empty(t, cause)
	require.True(t, reading.loggedIn)
	require.Equal(t, authAccountIdentityOf(seams.account), reading.identity)

	seams.statusExt = 1
	reading, cause = broker.readAccount(t.Context())
	require.Empty(t, cause)
	require.False(t, reading.loggedIn)

	seams.statusErr = context.DeadlineExceeded
	_, cause = broker.readAccount(t.Context())
	require.Equal(t, authCauseTimeout, cause)
}

func TestAccountReadingCurrentSignalIsClosed(t *testing.T) {
	resident := authAccountReading{
		identity: authAccountIdentityOf(claude.AuthAccount{LoggedIn: true, Email: "resident@example.test"}),
		loggedIn: true,
	}
	switched := authAccountReading{
		identity: authAccountIdentityOf(claude.AuthAccount{LoggedIn: true, Email: "switched@example.test"}),
		loggedIn: true,
	}
	empty := authAccountReading{}
	require.False(t, resident.advancedPast(resident))
	require.False(t, empty.advancedPast(resident))
	require.True(t, resident.advancedPast(empty))
	require.True(t, switched.advancedPast(resident))

	typ := reflect.TypeOf(authAccountReading{})
	require.True(t, typ.Comparable())
	for index := range typ.NumField() {
		field := typ.Field(index)
		require.True(t, field.Type.Comparable())
		require.NotContains(t, []reflect.Kind{reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface}, field.Type.Kind())
	}
}

func TestProviderNativeRemovalAndUserCurrentBehavior(t *testing.T) {
	require.Equal(t, "overlay", authNativeUser(claude.Options{Env: map[string]string{"USER": "overlay"}}))
	require.Equal(t, "ordinary", authNativeUser(claude.Options{OrdinaryEnvironment: map[string]string{"USER": "ordinary"}}))
	require.Equal(t, "managed", authNativeUser(claude.Options{Authority: &claude.NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"USER": "managed"} },
	}}))
	require.Empty(t, authNativeUser(claude.Options{}))

	seams := newAuthSeams(t)
	home := t.TempDir()
	broker, _ := newAuthBroker(t, WithHome(home), WithProviderAuthDirectHome(home))

	require.NoError(t, broker.removeKeystoreItems(t.Context()))
	require.Equal(t, 1, seams.removeCalls)
	require.Equal(t, "canary-user", seams.removedUser)
	resolved, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, resolved, seams.removedDir)

	seams.removeErr = errTestRandom
	requireAuthFailed(t, broker.removeKeystoreItems(t.Context()), authCauseTransport)
	seams.removeErr = ErrHostAuthorityUnavailable
	requireAuthFailed(t, broker.removeKeystoreItems(t.Context()), authCauseProcess)
}

func TestAuthLoginHandleCurrentFence(t *testing.T) {
	broker, _ := newAuthBroker(t)
	(*authLoginHandle)(nil).close()

	child := &fakeAuthLogin{}
	handle := &authLoginHandle{login: child, agent: broker.agent}
	require.False(t, handle.exited())
	handle.close()
	require.Equal(t, 1, child.closeCount())

	incomplete := &fakeAuthLogin{closeErr: ErrContainmentIncomplete}
	(&authLoginHandle{login: incomplete, agent: broker.agent}).close()
	require.ErrorIs(t, broker.agent.containmentErr, ErrContainmentIncomplete)

	child.submitErr = errTestRandom
	require.ErrorIs(t, handle.submit("value"), errTestRandom)
}

func TestAuthLoginHandleRetainsBusyCleanupForRetry(t *testing.T) {
	broker, _ := newAuthBroker(t)
	child := &fakeAuthLogin{closeErr: ErrNativeTreeBusy}
	handle := &authLoginHandle{login: child, agent: broker.agent, owner: broker}

	require.ErrorIs(t, handle.fence(), ErrNativeTreeBusy)
	broker.mu.Lock()
	_, retained := broker.retainedLogins[handle]
	broker.mu.Unlock()
	require.True(t, retained)

	child.mu.Lock()
	child.closeErr = nil
	child.mu.Unlock()
	require.NoError(t, broker.retryRetainedLogins())
	broker.mu.Lock()
	_, retained = broker.retainedLogins[handle]
	broker.mu.Unlock()
	require.False(t, retained)
	require.Equal(t, 2, child.closeCount())

	cleanupComplete := false
	terminalError := &fakeAuthLogin{closeErr: ErrHostAuthorityUnavailable, cleanupPending: &cleanupComplete}
	terminalHandle := &authLoginHandle{login: terminalError, agent: broker.agent, owner: broker}
	require.ErrorIs(t, terminalHandle.fence(), ErrHostAuthorityUnavailable)
	broker.mu.Lock()
	_, retained = broker.retainedLogins[terminalHandle]
	broker.mu.Unlock()
	require.False(t, retained)
}

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

func TestProviderAuthSharesPreparedHomeWithoutFilesystemAccess(t *testing.T) {
	authority := newFakeHostAuthority()
	authority.hidden = make(map[string]string)
	broker, _ := newAuthBroker(t, WithHostAuthority(authority))
	home := broker.home.path

	first := broker.claudeAuthority()
	second := broker.claudeAuthority()
	require.NoError(t, first.PrepareNativeTree(t.Context(), home))
	_, err := os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, second.PrepareNativeTree(t.Context(), home))
	require.Equal(t, []string{"prepare:" + home}, authority.snapshot())

	require.NoError(t, first.ReclaimNativeTree(t.Context(), home))
	_, err = os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []string{"prepare:" + home}, authority.snapshot())

	authority.settled = true
	require.NoError(t, second.ReclaimNativeTree(t.Context(), home))
	require.DirExists(t, home)
	require.Equal(t, []string{"prepare:" + home, "reclaim:" + home}, authority.snapshot())
}

func TestProviderAuthBusyReclaimIsRetryableWithoutContainmentLatch(t *testing.T) {
	authority := newFakeHostAuthority()
	broker, _ := newAuthBroker(t, WithHostAuthority(authority))
	home := broker.home.path
	native := broker.claudeAuthority()

	require.NoError(t, native.PrepareNativeTree(t.Context(), home))
	authority.reclaim = ErrNativeTreeBusy
	require.ErrorIs(t, native.ReclaimNativeTree(t.Context(), home), ErrNativeTreeBusy)
	require.Nil(t, broker.agent.containmentErr)

	require.NoError(t, native.PrepareNativeTree(t.Context(), home))
	authority.reclaim = nil
	require.NoError(t, native.ReclaimNativeTree(context.Background(), home))
	require.Nil(t, broker.agent.containmentErr)
	require.Equal(t, []string{"prepare:" + home, "reclaim:" + home, "reclaim:" + home}, authority.snapshot())
}

func TestProviderAuthRemovalRefusesBeforeStatWhileHomeIsPrepared(t *testing.T) {
	authority := newFakeHostAuthority()
	authority.hidden = make(map[string]string)
	broker, _ := newAuthBroker(t, WithHostAuthority(authority))
	home := broker.home.path
	native := broker.claudeAuthority()

	require.NoError(t, native.PrepareNativeTree(t.Context(), home))
	_, err := broker.nativeRemovalOptions(t.Context())
	require.ErrorIs(t, err, ErrNativeTreeBusy)

	authority.settled = true
	require.NoError(t, native.ReclaimNativeTree(t.Context(), home))
	_, err = broker.nativeRemovalOptions(t.Context())
	require.NoError(t, err)
}

func TestProviderAuthRemovalClassifiesUncertainReclaim(t *testing.T) {
	authority := newFakeHostAuthority()
	authority.reclaim = errTestRandom
	broker, _ := newAuthBroker(t, WithHostAuthority(authority))
	broker.nativeTreePrepared = true

	_, err := broker.nativeRemovalOptions(t.Context())
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorIs(t, broker.agent.containmentErr, ErrContainmentIncomplete)
}
