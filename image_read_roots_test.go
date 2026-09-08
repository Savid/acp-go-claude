package claudeacp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestManagedMediaReadsUseDisjointPinnedRoots(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home, scratch, workspace, handoff := filepath.Join(parent, "home"), filepath.Join(parent, "scratch"), filepath.Join(parent, "workspace"), filepath.Join(parent, "handoff")
	for _, root := range []string{home, scratch, workspace, handoff} {
		require.NoError(t, os.Mkdir(root, 0o700))
	}
	authority := newFakeHostAuthority()
	agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(handoff))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	agent.managedImages.prepare(workspace, os.TempDir())
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), home))

	data := outputFixtureBytes(t, "valid.png")
	for _, root := range []string{home, scratch, workspace, handoff} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "image.png"), data, 0o600))
	}
	session := &agentSession{agent: agent, cwd: workspace}
	got, err := session.readAllowedImageFile(t.Context(), filepath.Join(workspace, "image.png"))
	require.NoError(t, err)
	require.Equal(t, data, got)
	file, err := agent.handoffImageReader().OpenHandoffImage(t.Context(), filepath.Join(handoff, "image.png"))
	require.NoError(t, err)
	got, err = io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, data, got)

	// Later native allocation needs no new path classification: the complete
	// scratch domain was already excluded before the first preparation.
	later := filepath.Join(scratch, "later")
	require.NoError(t, os.Mkdir(later, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(later, "image.png"), data, 0o600))
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), later))
	for _, path := range []string{filepath.Join(home, "image.png"), filepath.Join(scratch, "image.png"), filepath.Join(later, "image.png")} {
		_, err = session.readAllowedImageFile(t.Context(), path)
		requireImageOutputError(t, err, imageOutputPathNotAllowed)
	}

	// A later workspace can derive a narrower root through an existing handle.
	child := filepath.Join(workspace, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(child, "image.png"), data, 0o600))
	session.cwd = child
	got, err = session.readAllowedImageFile(t.Context(), filepath.Join(child, "image.png"))
	require.NoError(t, err)
	require.Equal(t, data, got)

	// A link created after preparation cannot escape that retained boundary.
	link := filepath.Join(child, "escape.png")
	require.NoError(t, os.Symlink(filepath.Join(home, "image.png"), link))
	_, err = session.readAllowedImageFile(t.Context(), link)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
	_, err = session.readAllowedImageFile(t.Context(), filepath.Join(workspace, "image.png"))
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
}

func TestManagedMediaRejectsOverlappingRootIdentitiesAndFreezesOnFailedPrepare(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home, scratch := filepath.Join(parent, "home"), filepath.Join(parent, "scratch")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.Mkdir(scratch, 0o700))
	alias := filepath.Join(parent, "home-alias")
	require.NoError(t, os.Symlink(home, alias))
	data := outputFixtureBytes(t, "valid.png")
	require.NoError(t, os.WriteFile(filepath.Join(home, "image.png"), data, 0o600))
	authority := newFakeHostAuthority()
	authority.prepare = errors.New("prepare response lost")
	agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(alias))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	agent.managedImages.prepare(parent, home, scratch)
	require.Empty(t, agent.managedImages.roots)
	require.Error(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), home))

	laterWorkspace := t.TempDir()
	agent.managedImages.prepare(laterWorkspace)
	require.Empty(t, agent.managedImages.roots)
	_, err := agent.handoffImageReader().OpenHandoffImage(t.Context(), filepath.Join(alias, "image.png"))
	var refused *mapper.HandoffPathError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, mapper.HandoffPathNotAllowed, refused.Verdict)
}

func TestManagedMediaClosePreventsLateAuthorityCallsFromReopeningRoots(t *testing.T) {
	t.Parallel()
	handoff, scratch := t.TempDir(), t.TempDir()
	agent := NewAgent(WithHostAuthority(newFakeHostAuthority()), WithScratchDir(scratch), WithInputHandoffRoot(handoff))
	require.NoError(t, agent.Close())
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), scratch))
	require.False(t, agent.managedImages.initialized)
	require.Empty(t, agent.managedImages.roots)
	_, err := agent.managedImages.open(filepath.Join(handoff, "image.png"), []string{handoff})
	require.ErrorIs(t, err, errManagedImageRoot)
}

func TestManagedMediaPinsRootAliasesBeforeEvenAFailedNativeProbe(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home, scratch, workspace := filepath.Join(parent, "home"), filepath.Join(parent, "scratch"), filepath.Join(parent, "workspace")
	for _, root := range []string{home, scratch, workspace} {
		require.NoError(t, os.Mkdir(root, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "image.png"), []byte("native-private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "image.png"), []byte("workspace-image"), 0o600))
	alias := filepath.Join(parent, "workspace-alias")
	require.NoError(t, os.Symlink(workspace, alias))
	authority := newFakeHostAuthority()
	authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
		return nil, errors.New("native probe failed")
	}
	agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(scratch))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	agent.managedImages.prepare(alias)
	_, err := agent.options.HostAuthority.StartNative(t.Context(), NativeRequest{})
	require.Error(t, err)
	later := t.TempDir()
	agent.managedImages.prepare(later)
	require.NotContains(t, agent.managedImages.roots, later)

	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(home, alias))
	session := &agentSession{agent: agent, cwd: alias}
	data, err := session.readAllowedImageFile(t.Context(), filepath.Join(alias, "image.png"))
	require.NoError(t, err)
	require.Equal(t, "workspace-image", string(data))
}
