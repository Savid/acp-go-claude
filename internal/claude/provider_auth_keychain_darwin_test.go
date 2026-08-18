package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveAuthKeychainItemsRemovesEveryItemUnderABoundedCall(t *testing.T) {
	original := authKeychainTool

	t.Cleanup(func() { authKeychainTool = original })

	var calls [][]string

	authKeychainTool = func(ctx context.Context, args []string, _ Options) (int, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "every keystore call carries a bound")
		require.False(t, deadline.IsZero())

		calls = append(calls, args)

		return 0, nil
	}

	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))
	require.Len(t, calls, 4)
	require.Equal(t, "delete-generic-password", calls[0][0])
	require.Equal(t, "-a", calls[0][3])
	require.Equal(t, "operator", calls[0][4])
}

func TestRemoveAuthKeychainItemsSeparatesAbsenceFromTransientFailure(t *testing.T) {
	original := authKeychainTool

	t.Cleanup(func() { authKeychainTool = original })

	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 44, nil }
	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))

	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 1, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), "status 1")

	// A keychain that refuses the delete answers 51 with the item still in it.
	// Reported as success, the caller would tell the operator a credential was
	// cleared that a later login still finds.
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 51, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), "status 51")

	want := errors.New("tool missing")
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 0, want }
	require.ErrorIs(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), want)
}

func TestReadAuthKeychainCredentialReturnsTheFirstPresentItemUnderABoundedCall(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	var calls [][]string

	authKeychainReadTool = func(ctx context.Context, args []string, _ Options) ([]byte, int, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "every keystore call carries a bound")
		require.False(t, deadline.IsZero())

		calls = append(calls, args)
		if len(calls) == 1 {
			return nil, 44, nil
		}

		// The platform tool appends one newline the stored blob never carries.
		return []byte(`{"claudeAiOauth":{"accessToken":"unit-secret"}}` + "\n"), 0, nil
	}

	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Equal(t, `{"claudeAiOauth":{"accessToken":"unit-secret"}}`, string(data))
	require.Len(t, calls, 2)
	require.Equal(t, "find-generic-password", calls[0][0])
	require.Equal(t, "-a", calls[0][3])
	require.Equal(t, "operator", calls[0][4])
	require.Equal(t, "-w", calls[0][5])
}

func TestReadAuthKeychainCredentialAnswersAbsenceForMissingAndEmptyItems(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 44, nil }
	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Nil(t, data)

	// An item holding nothing is absence, not a credential.
	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return []byte("\n"), 0, nil }
	data, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Nil(t, data)
}

func TestReadAuthKeychainCredentialSeparatesAbsenceFromTransientFailure(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 51, nil }
	_, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.ErrorContains(t, err, "status 51")

	want := errors.New("tool missing")
	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 0, want }
	_, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.ErrorIs(t, err, want)
}

func TestReadAuthKeychainCredentialPrefersALaterItemOverAnEarlierFailure(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	var calls int

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) {
		calls++
		if calls == 1 {
			return nil, 51, nil
		}

		return []byte(`{"claudeAiOauth":{}}`), 0, nil
	}

	// A credential that is actually there beats reporting the first item's
	// refusal: the session either resumes logged in or it does not.
	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Equal(t, `{"claudeAiOauth":{}}`, string(data))
}

func TestAuthKeychainExitCodeSeparatesAnAnswerFromALaunchFailure(t *testing.T) {
	t.Parallel()

	code, err := authKeychainExitCode(nil)
	require.NoError(t, err)
	require.Zero(t, code)

	exitErr := exec.Command("/bin/sh", "-c", "exit 44").Run()
	require.Error(t, exitErr)

	code, err = authKeychainExitCode(exitErr)
	require.NoError(t, err)
	require.Equal(t, 44, code)
	require.True(t, authKeychainAbsent(code))

	want := errors.New("tool missing")

	code, err = authKeychainExitCode(fmt.Errorf("launch: %w", want))
	require.ErrorIs(t, err, want)
	require.Zero(t, code)
}

func TestAuthKeychainReadResultSurfacesTheBlobOnlyForAPresentItem(t *testing.T) {
	t.Parallel()

	output, code, err := authKeychainReadResult([]byte("blob\n"), nil)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Equal(t, "blob\n", string(output))

	output, code, err = authKeychainReadResult([]byte("partial"), exec.Command("/bin/sh", "-c", "exit 51").Run())
	require.NoError(t, err)
	require.Equal(t, 51, code)
	require.Nil(t, output)

	want := errors.New("tool missing")

	output, code, err = authKeychainReadResult([]byte("partial"), want)
	require.ErrorIs(t, err, want)
	require.Zero(t, code)
	require.Nil(t, output)
}

func TestAuthKeychainToolsRequireContainmentAndNativeAdmission(t *testing.T) {
	_, _, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, Options{ProcessIsolation: &ProcessIsolation{}})
	require.ErrorContains(t, err, "invalid process isolation")

	isolation := testProcessIsolation()
	isolation.BaseEnvironment[envSearchPath] = t.TempDir()

	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, Options{ProcessIsolation: isolation})
	require.ErrorIs(t, err, exec.ErrNotFound)

	options := withTestProcessIsolation(Options{})
	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	// Best-effort containment consumes the generation and the native-root
	// permit, so hooks that are not wired are an incomplete boundary there.
	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, Options{
		DarwinBestEffort:    true,
		OrdinaryEnvironment: map[string]string{envSearchPath: "/usr/bin:/bin"},
	})
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorContains(t, err, "keychain native admission is unavailable")

	useAuthDirectContainment(t)
	prepared := 0
	acquired := 0
	released := 0
	options.DarwinBestEffort = true
	options.PrepareKeychainGeneration = func(context.Context) (*DarwinGeneration, error) {
		prepared++

		return &DarwinGeneration{ScratchRoot: t.TempDir()}, nil
	}
	options.AcquireKeychainDiscovery = func(context.Context) (func(), error) {
		acquired++

		return func() { released++ }, nil
	}
	_, code, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Equal(t, 1, prepared)
	require.Equal(t, 1, acquired)
	require.Equal(t, 1, released)
}

// TestAuthKeychainToolsRunOrdinarilyWithoutAdmissionHooks pins the ordinary
// same-identity mode: the platform tool launches directly, consuming neither a
// containment generation nor a native-root permit, so absent hooks are not an
// incomplete boundary. This is the mode a plain darwin host resumes sessions
// in, and the read leg it uses to carry a native login's Keychain credential
// into a materialized temp home.
func TestAuthKeychainToolsRunOrdinarilyWithoutAdmissionHooks(t *testing.T) {
	t.Parallel()

	output, code, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, Options{
		OrdinaryEnvironment: map[string]string{envSearchPath: "/usr/bin:/bin"},
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.NotEmpty(t, output)
}

func TestContainedAuthKeychainToolFailureBranches(t *testing.T) {
	useAuthDirectContainment(t)
	directPrepare := processPrepareContained
	options := withTestProcessIsolation(Options{DarwinBestEffort: true})
	options.PrepareKeychainGeneration = func(context.Context) (*DarwinGeneration, error) {
		return &DarwinGeneration{ScratchRoot: t.TempDir()}, nil
	}
	releases := 0
	options.AcquireKeychainDiscovery = func(context.Context) (func(), error) {
		return func() { releases++ }, nil
	}

	wantPrepare := errors.New("prepare generation")
	prepare := options.PrepareKeychainGeneration
	options.PrepareKeychainGeneration = func(context.Context) (*DarwinGeneration, error) { return nil, wantPrepare }
	_, _, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.ErrorIs(t, err, wantPrepare)
	options.PrepareKeychainGeneration = prepare

	wantAcquire := errors.New("acquire native root")
	acquire := options.AcquireKeychainDiscovery
	finished := false
	options.PrepareKeychainGeneration = func(context.Context) (*DarwinGeneration, error) {
		return &DarwinGeneration{Release: func(complete bool) error {
			finished = complete

			return nil
		}}, nil
	}
	options.AcquireKeychainDiscovery = func(context.Context) (func(), error) { return nil, wantAcquire }
	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.ErrorIs(t, err, wantAcquire)
	require.True(t, finished)
	options.PrepareKeychainGeneration = prepare
	options.AcquireKeychainDiscovery = acquire

	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, ErrProcessContainmentIncomplete
	}
	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Zero(t, releases)

	wantRun := errors.New("prepare containment")
	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) { return nil, wantRun }
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = runContainedAuthKeychainTool(canceled, []string{"list-keychains"}, options)
	require.ErrorIs(t, err, context.Canceled)

	_, _, err = runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.ErrorIs(t, err, wantRun)

	processPrepareContained = func(command *exec.Cmd, launchOptions processLaunchOptions) (*processTreeCommand, error) {
		command.Path = "/bin/sh"
		command.Args = []string{"sh", "-c", "exit 51"}

		return directPrepare(command, launchOptions)
	}
	_, code, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.NoError(t, err)
	require.Equal(t, 51, code)
}

func TestAuthKeychainToolsReportALaunchFailureAsOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged session so the credential cannot be applied")
	}

	t.Parallel()

	options := withTestProcessIsolation(Options{})

	code, err := authKeychainTool(t.Context(), []string{"list-keychains"}, options)
	require.Error(t, err)
	require.Zero(t, code)

	output, code, err := authKeychainReadTool(t.Context(), []string{"list-keychains"}, options)
	require.Error(t, err)
	require.Zero(t, code)
	require.Nil(t, output)

	_, err = authKeychainTool(t.Context(), []string{"list-keychains"}, Options{ProcessIsolation: &ProcessIsolation{}})
	require.ErrorContains(t, err, "invalid process isolation")

	_, _, err = authKeychainReadTool(t.Context(), []string{"list-keychains"}, Options{ProcessIsolation: &ProcessIsolation{}})
	require.ErrorContains(t, err, "invalid process isolation")
}
