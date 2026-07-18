package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestContainmentModeAndValidationAcrossPlatforms(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	runtimeGOOS = "linux"
	if got := containmentMode(Options{}); got != RuntimeContainmentAuthoritative {
		t.Fatalf("Linux mode = %q", got)
	}
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentUnavailable {
		t.Fatalf("off-Darwin opt-in mode = %q", got)
	}
	if err := validateContainmentOptions(Options{DarwinBestEffortContainment: true}); err == nil {
		t.Fatal("off-Darwin opt-in was accepted")
	}

	runtimeGOOS = "windows"
	if got := containmentMode(Options{}); got != RuntimeContainmentAuthoritative {
		t.Fatalf("Windows mode = %q", got)
	}

	runtimeGOOS = "darwin"
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentBestEffort {
		t.Fatalf("Darwin opt-in mode = %q", got)
	}
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("Darwin default mode = %q", got)
	}
	if err := validateContainmentOptions(Options{Env: map[string]string{"acp_go_claude_internal_bad": "value"}}); err == nil {
		t.Fatal("reserved private environment was accepted")
	}
	if err := validateContainmentOptions(Options{}); err != nil {
		t.Fatal(err)
	}

	runtimeGOOS = "freebsd"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("unsupported mode = %q", got)
	}
}

func TestAgentContainmentModeObservationAndWarning(t *testing.T) {
	if got := (*Agent)(nil).ContainmentMode(); got != RuntimeContainmentUnavailable {
		t.Fatalf("nil agent mode = %q", got)
	}

	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })
	runtimeGOOS = "darwin"

	var observed []RuntimeContainmentMode
	var logs bytes.Buffer
	agent := NewAgent(
		WithDarwinBestEffortContainment(),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
				observed = append(observed, mode)
			},
		}),
	)
	if agent.ContainmentMode() != RuntimeContainmentBestEffort {
		t.Fatalf("mode = %q", agent.ContainmentMode())
	}
	if len(observed) != 1 || observed[0] != RuntimeContainmentBestEffort {
		t.Fatalf("observations = %v", observed)
	}
	if !strings.Contains(logs.String(), `"containment":"best_effort"`) || !strings.Contains(logs.String(), "escaped descendants may survive") {
		t.Fatalf("warning = %q", logs.String())
	}

	runtimeGOOS = "linux"
	invalid := NewAgent(WithDarwinBestEffortContainment())
	if _, err := invalid.Initialize(t.Context(), acp.InitializeRequest{}); err == nil || !strings.Contains(err.Error(), "supported only on darwin") {
		t.Fatalf("off-Darwin initialization error = %v", err)
	}
}

func TestPrepareDarwinGenerationResources(t *testing.T) {
	originalMkdir := mkdirDarwinGeneration
	originalRemove := removeDarwinGeneration
	originalChmod := chmodDarwinGeneration
	t.Cleanup(func() {
		mkdirDarwinGeneration = originalMkdir
		removeDarwinGeneration = originalRemove
		chmodDarwinGeneration = originalChmod
	})

	if generation, err := (*Agent)(nil).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || generation != nil {
		t.Fatalf("nil agent generation=%v err=%v", generation, err)
	}
	if generation, err := NewAgent().prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || generation != nil {
		t.Fatalf("authoritative agent generation=%v err=%v", generation, err)
	}

	want := errors.New("resource")
	newAgent := func(parent string, reserve func(context.Context, RuntimeResourceKind) (func(), error)) *Agent {
		original := runtimeGOOS
		runtimeGOOS = "darwin"
		agent := NewAgent(
			WithScratchDir(parent),
			WithDarwinBestEffortContainment(),
			WithRuntimeResourceHooks(RuntimeResourceHooks{ReserveScratchRoot: reserve}),
		)
		runtimeGOOS = original

		return agent
	}

	agent := newAgent(t.TempDir(), func(context.Context, RuntimeResourceKind) (func(), error) { return nil, want })
	if _, err := agent.prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) {
		t.Fatalf("reserve error = %v", err)
	}

	reserved := 0
	released := 0
	reserve := func(context.Context, RuntimeResourceKind) (func(), error) {
		reserved++

		return func() { released++ }, nil
	}
	fileParent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent(fileParent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || reserved != 1 || released != 1 {
		t.Fatalf("parent error=%v reserved=%d released=%d", err, reserved, released)
	}

	parent := t.TempDir()
	mkdirDarwinGeneration = func(string, string) (string, error) { return "", want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) || released != 2 {
		t.Fatalf("mkdir error=%v released=%d", err, released)
	}
	mkdirDarwinGeneration = originalMkdir

	chmodDarwinGeneration = func(string, os.FileMode) error { return want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) || released != 3 {
		t.Fatalf("chmod error=%v released=%d", err, released)
	}
	chmodDarwinGeneration = originalChmod

	registry := filepath.Join(parent, "acp-go-claude-containment")
	if err := os.WriteFile(registry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || released != 4 {
		t.Fatalf("record error=%v released=%d", err, released)
	}
	removeDarwinGeneration = func(string) error { return want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || released != 4 {
		t.Fatalf("record/remove error=%v released=%d", err, released)
	}
	removeDarwinGeneration = originalRemove
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}

	generation, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt)
	if err != nil {
		t.Fatal(err)
	}
	beforeRelease := released
	releaseErr := generation.Release(false)
	if releaseErr != nil || released != beforeRelease {
		t.Fatalf("incomplete release error=%v releases=%d", releaseErr, released)
	}
	removeDarwinGeneration = func(string) error { return want }
	releaseErr = generation.Release(true)
	if !errors.Is(releaseErr, want) || released != beforeRelease {
		t.Fatalf("failed complete release error=%v releases=%d", releaseErr, released)
	}
	removeDarwinGeneration = originalRemove

	generation, err = newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt)
	if err != nil {
		t.Fatal(err)
	}
	releaseErr = generation.Release(true)
	if releaseErr != nil || released != beforeRelease+1 {
		t.Fatalf("complete release error=%v releases=%d", releaseErr, released)
	}
}

func TestPrepareUsageGenerationResources(t *testing.T) {
	originalMkdir := mkdirDarwinGeneration
	originalRemove := removeDarwinGeneration
	originalChmod := chmodDarwinGeneration
	t.Cleanup(func() {
		mkdirDarwinGeneration = originalMkdir
		removeDarwinGeneration = originalRemove
		chmodDarwinGeneration = originalChmod
	})

	if generation, err := (*Agent)(nil).prepareUsageGeneration(t.Context()); err == nil || generation != nil {
		t.Fatalf("nil agent generation=%v err=%v", generation, err)
	}

	bestEffort := NewAgent(WithScratchDir(t.TempDir()))
	bestEffort.containmentMode = RuntimeContainmentBestEffort
	generation, err := bestEffort.prepareUsageGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(true))

	unavailable := NewAgent()
	unavailable.containmentMode = RuntimeContainmentUnavailable
	_, err = unavailable.prepareUsageGeneration(t.Context())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	wantErr := errors.New("usage generation failure")
	authoritative := func(parent string, reserve func(context.Context, RuntimeResourceKind) (func(), error)) *Agent {
		agent := NewAgent(
			WithScratchDir(parent),
			WithRuntimeResourceHooks(RuntimeResourceHooks{ReserveScratchRoot: reserve}),
		)
		agent.containmentMode = RuntimeContainmentAuthoritative

		return agent
	}
	_, err = authoritative(t.TempDir(), func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, wantErr
	}).prepareUsageGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)

	releases := 0
	reserve := func(context.Context, RuntimeResourceKind) (func(), error) {
		return func() { releases++ }, nil
	}
	fileParent := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(fileParent, nil, 0o600))
	_, err = authoritative(fileParent, reserve).prepareUsageGeneration(t.Context())
	require.Error(t, err)
	require.Equal(t, 1, releases)

	parent := t.TempDir()
	mkdirDarwinGeneration = func(string, string) (string, error) { return "", wantErr }
	_, err = authoritative(parent, reserve).prepareUsageGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 2, releases)
	mkdirDarwinGeneration = originalMkdir

	chmodDarwinGeneration = func(string, os.FileMode) error { return wantErr }
	_, err = authoritative(parent, reserve).prepareUsageGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 3, releases)
	chmodDarwinGeneration = originalChmod

	generation, err = authoritative(parent, reserve).prepareUsageGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(false))
	require.Equal(t, 3, releases)
	removeDarwinGeneration = func(string) error { return wantErr }
	require.ErrorIs(t, generation.Release(true), wantErr)
	require.Equal(t, 3, releases)
	removeDarwinGeneration = originalRemove

	generation, err = authoritative(parent, reserve).prepareUsageGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(true))
	require.Equal(t, 4, releases)
}
