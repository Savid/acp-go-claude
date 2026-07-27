package claudeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestNativeOptionsRunsFlowsInTheSessionHome(t *testing.T) {
	home := t.TempDir()
	broker, _ := newAuthBroker(t, WithHome(home), WithExecutablePath("/bin/claude"), WithEnv(map[string]string{"A": "B"}))

	options, err := broker.nativeOptions()
	require.NoError(t, err)
	require.Equal(t, "/bin/claude", options.CLIPath)
	require.Equal(t, map[string]string{"A": "B"}, options.Env)
	require.False(t, options.DarwinBestEffort)

	resolved, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, resolved, options.ClaudeHome)

	broker.agent.containmentMode = RuntimeContainmentBestEffort

	bestEffort, err := broker.nativeOptions()
	require.NoError(t, err)
	require.True(t, bestEffort.DarwinBestEffort)
}

func TestNativeOptionsFailsOnAnUnresolvableHome(t *testing.T) {
	broker, _ := newAuthBroker(t, WithHome(filepath.Join(t.TempDir(), "absent")))

	_, err := broker.nativeOptions()
	require.Error(t, err)
}

func TestNativeLegsFailClosedOnAnUnresolvableHome(t *testing.T) {
	newAuthSeams(t)

	broker, _ := newAuthBroker(t, WithHome(filepath.Join(t.TempDir(), "absent")))

	_, _, err := broker.probeAccount(t.Context())
	requireAuthFailed(t, err, authCauseProcess)

	requireAuthFailed(t, broker.nativeLogout(t.Context()), authCauseProcess)
	requireAuthFailed(t, broker.removeKeystoreItems(t.Context()), authCauseProcess)

	_, _, err = broker.startLogin(t.Context())
	requireAuthFailed(t, err, authCauseProcess)
}

func TestNativeLegsFailClosedWhenAdmissionIsRefused(t *testing.T) {
	newAuthSeams(t)

	broker, _ := newAuthBroker(t, WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return nil, errTestRandom
		},
	}))
	broker.agent.containmentMode = RuntimeContainmentAuthoritative

	_, _, err := broker.probeAccount(t.Context())
	requireAuthFailed(t, err, authCauseProcess)

	requireAuthFailed(t, broker.nativeLogout(t.Context()), authCauseProcess)

	_, _, err = broker.startLogin(t.Context())
	requireAuthFailed(t, err, authCauseProcess)
}

func TestAuthNativeAdmissionFailsWhenNoGenerationCanBePrepared(t *testing.T) {
	broker, _ := newAuthBroker(t)
	broker.agent.containmentMode = RuntimeContainmentUnavailable

	_, _, err := broker.authNativeAdmission(t.Context())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}

func TestAuthNativeCauseClassifiesWithoutForwardingText(t *testing.T) {
	broker, _ := newAuthBroker(t)

	require.Equal(t, authCauseProcess, broker.authNativeCause(claude.ErrProcessContainmentIncomplete))
	require.ErrorIs(t, broker.agent.containmentErr, claude.ErrProcessContainmentIncomplete)

	require.Equal(t, authCauseTimeout, broker.authNativeCause(context.DeadlineExceeded))
	require.Equal(t, authCauseProcess, broker.authNativeCause(errTestRandom))
}

func TestProbeAccountReadsTheExitCodeAndTheLoggedInFlag(t *testing.T) {
	seams := newAuthSeams(t)
	broker, _ := newAuthBroker(t)

	account, present, err := broker.probeAccount(t.Context())
	require.NoError(t, err)
	require.True(t, present)
	require.True(t, account.LoggedIn)

	seams.statusExt = 1

	_, present, err = broker.probeAccount(t.Context())
	require.NoError(t, err)
	require.False(t, present)

	seams.statusExt = 0
	seams.account = claude.AuthAccount{LoggedIn: false, AuthMethod: "oauth_token"}

	_, present, err = broker.probeAccount(t.Context())
	require.NoError(t, err)
	require.False(t, present)
}

func TestNativeSeamsHoldTheContainmentPermitOnAnIncompleteBoundary(t *testing.T) {
	seams := newAuthSeams(t)

	released := 0
	broker, _ := newAuthBroker(t, WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { released++ }, nil
		},
	}))
	broker.agent.containmentMode = RuntimeContainmentAuthoritative

	seams.statusErr = claude.ErrProcessContainmentIncomplete

	_, _, err := broker.probeAccount(t.Context())
	requireAuthFailed(t, err, authCauseProcess)
	require.Zero(t, released)

	seams.logoutErr = claude.ErrProcessContainmentIncomplete
	requireAuthFailed(t, broker.nativeLogout(t.Context()), authCauseProcess)
	require.Zero(t, released)

	seams.loginErr = claude.ErrProcessContainmentIncomplete

	_, _, err = broker.startLogin(t.Context())
	requireAuthFailed(t, err, authCauseProcess)
	require.Zero(t, released)
}

func TestRemoveKeystoreItemsNamesTheConfigDirAndTheAccount(t *testing.T) {
	seams := newAuthSeams(t)

	home := t.TempDir()
	broker, _ := newAuthBroker(t, WithHome(home))

	require.NoError(t, broker.removeKeystoreItems(t.Context()))
	require.Equal(t, 1, seams.removeCalls)
	require.Equal(t, "canary-user", seams.removedUser)

	resolved, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, resolved, seams.removedDir)

	seams.removeErr = errTestRandom
	requireAuthFailed(t, broker.removeKeystoreItems(t.Context()), authCauseTransport)
}

func TestAuthNativeUserReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("USER", "operator")
	require.Equal(t, "operator", authNativeUser())
}

func TestAuthLoginBeginWrapsTheNativeStart(t *testing.T) {
	original := authLoginStart

	authLoginStart = func(context.Context, claude.Options, *claude.DarwinGeneration) (*claude.AuthLogin, string, error) {
		return nil, "", errTestRandom
	}

	t.Cleanup(func() { authLoginStart = original })

	_, _, err := authLoginBegin(t.Context(), claude.Options{}, nil)
	require.ErrorIs(t, err, errTestRandom)

	authLoginStart = func(context.Context, claude.Options, *claude.DarwinGeneration) (*claude.AuthLogin, string, error) {
		return &claude.AuthLogin{}, "https://claude.com/", nil
	}

	login, url, err := authLoginBegin(t.Context(), claude.Options{}, nil)
	require.NoError(t, err)
	require.NotNil(t, login)
	require.Equal(t, "https://claude.com/", url)
}

func TestAuthLoginHandleClose(t *testing.T) {
	newAuthSeams(t)

	broker, _ := newAuthBroker(t)

	(*authLoginHandle)(nil).close()

	released := false
	child := &fakeAuthLogin{}
	handle := &authLoginHandle{login: child, release: func() { released = true }, agent: broker.agent}

	handle.close()
	require.True(t, released)
	require.Equal(t, 1, child.closeCount())

	incomplete := &fakeAuthLogin{closeErr: claude.ErrProcessContainmentIncomplete}
	held := &authLoginHandle{login: incomplete, release: func() { t.Fatal("permit released on an incomplete boundary") }, agent: broker.agent}

	held.close()
	require.ErrorIs(t, broker.agent.containmentErr, claude.ErrProcessContainmentIncomplete)
}

func TestAuthLoginHandleSubmitAndExited(t *testing.T) {
	child := &fakeAuthLogin{}
	handle := &authLoginHandle{login: child}

	require.NoError(t, handle.submit("value"))
	require.Equal(t, []string{"value"}, child.values())
	require.False(t, handle.exited())

	child.submitErr = errTestRandom
	require.ErrorIs(t, handle.submit("value"), errTestRandom)
}

func TestSessionCloseCancelsPendingProviderAuthFlows(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	startAuthFlow(t, broker, sessionID)

	session := broker.agent.sessions[sessionID]
	session.client = &claude.Client{}

	_ = session.Close(t.Context())

	require.Equal(t, 1, seams.login.closeCount())
}

func TestProviderAuthLedgerRootIsCreatedWithRestrictedModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "root")
	broker, _ := newAuthBroker(t, WithProviderAuthRoot(root))

	info, err := os.Stat(broker.ledger.dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	entries, err := os.ReadDir(broker.ledger.dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the writability probe leaves nothing behind")

	require.True(t, errors.Is(os.ErrNotExist, os.ErrNotExist))
}
