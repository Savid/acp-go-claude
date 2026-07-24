package claudeacp

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaterializeStoreSessionCarriesResumeCredentialPrivately(t *testing.T) {
	ctx := t.Context()
	sessionID := "77777777-7777-4777-8777-777777777777"
	cwd := t.TempDir()
	source := t.TempDir()
	credential := []byte(`{"claudeAiOauth":{"accessToken":"unit-secret"}}`)
	require.NoError(t, os.WriteFile(filepath.Join(source, claudeResumeCredentialFile), credential, 0o644))

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID}, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"hello"}}`),
	}))
	scratch := filepath.Join(t.TempDir(), "private", "scratch")
	agent := NewAgent(WithHome(source), WithSessionStore(store), WithScratchDir(scratch))
	materialized, err := agent.materializeStoreSession(ctx, sessionID, cwd, source, nil)
	require.NoError(t, err)
	require.NotNil(t, materialized)

	configInfo, err := os.Stat(materialized.configDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), configInfo.Mode().Perm())
	scratchInfo, err := os.Stat(scratch)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), scratchInfo.Mode().Perm())

	destination := filepath.Join(materialized.configDir, claudeResumeCredentialFile)
	copied, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, credential, copied)
	destinationInfo, err := os.Stat(destination)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), destinationInfo.Mode().Perm())

	stored, err := store.Load(ctx, SessionKey{SessionID: sessionID})
	require.NoError(t, err)
	for _, entry := range stored {
		require.NotContains(t, string(entry), "unit-secret")
	}
	transcript, err := os.ReadFile(materialized.mainPath)
	require.NoError(t, err)
	require.NotContains(t, string(transcript), "unit-secret")

	configDir := materialized.configDir
	require.NoError(t, materialized.Close())
	require.NoDirExists(t, configDir)
}

func TestCopyClaudeResumeCredentialMissingPreservesFirstTurnAuthContract(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()

	require.NoError(t, copyClaudeResumeCredential(source, destination))
	require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
}

func TestCopyClaudeResumeCredentialRejectsUnsafeSources(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		target := filepath.Join(t.TempDir(), "credential.json")
		require.NoError(t, os.WriteFile(target, []byte(`{"token":"unit-secret"}`), 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(source, claudeResumeCredentialFile)))

		err := copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialUnsafe)
		requireSecretSafeCredentialError(t, err, source, "unit-secret")
		require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
	})

	t.Run("nonregular", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(source, claudeResumeCredentialFile), 0o700))

		err := copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialUnsafe)
		requireSecretSafeCredentialError(t, err, source, "")
		require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
	})

	t.Run("oversized", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		file, err := os.OpenFile(filepath.Join(source, claudeResumeCredentialFile), os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		require.NoError(t, file.Truncate(claudeResumeCredentialMaxBytes+1))
		require.NoError(t, file.Close())

		err = copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialOversized)
		requireSecretSafeCredentialError(t, err, source, "")
		require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
	})

	t.Run("malformed", func(t *testing.T) {
		source := t.TempDir()
		scratch := filepath.Join(t.TempDir(), "scratch")
		secret := "unit-secret-malformed"
		require.NoError(t, os.WriteFile(
			filepath.Join(source, claudeResumeCredentialFile),
			[]byte(`{"accessToken":"`+secret+`"`),
			0o600,
		))
		store := NewInMemorySessionStore()
		sessionID := "99999999-9999-4999-8999-999999999999"
		require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: sessionID}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
		agent := NewAgent(WithHome(source), WithSessionStore(store), WithScratchDir(scratch))

		materialized, err := agent.materializeStoreSession(t.Context(), sessionID, t.TempDir(), source, nil)
		require.Nil(t, materialized)
		require.ErrorIs(t, err, errClaudeResumeCredentialMalformed)
		requireSecretSafeCredentialError(t, err, source, secret)
		requireScratchDirEmpty(t, scratch)
	})

	t.Run("unreadable", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		path := filepath.Join(source, claudeResumeCredentialFile)
		require.NoError(t, os.WriteFile(path, []byte(`{"token":"unit-secret"}`), 0o600))
		require.NoError(t, os.Chmod(path, 0))
		probe, probeErr := os.Open(path)
		if probeErr == nil {
			require.NoError(t, probe.Close())
			t.Skip("filesystem owner bypasses unreadable mode")
		}

		err := copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialUnreadable)
		requireSecretSafeCredentialError(t, err, source, "unit-secret")
		require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
	})
}

func TestCopyClaudeResumeCredentialRejectsMalformedJSONShapes(t *testing.T) {
	for name, content := range map[string]string{
		"empty": "", "null": "null", "array": "[]", "string": `"string"`, "invalid": "{bad}",
	} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			destination := t.TempDir()
			require.NoError(t, os.WriteFile(
				filepath.Join(source, claudeResumeCredentialFile), []byte(content), 0o600,
			))

			err := copyClaudeResumeCredential(source, destination)
			require.ErrorIs(t, err, errClaudeResumeCredentialMalformed)
			requireSecretSafeCredentialError(t, err, source, content)
		})
	}
}

func TestCopyClaudeResumeCredentialRejectsUnsafeDestinations(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(source, claudeResumeCredentialFile), []byte(`{"token":"unit-secret"}`), 0o600,
	))

	t.Run("symlink root", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "resume")
		require.NoError(t, os.Symlink(t.TempDir(), destination))

		err := copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialDestination)
		requireSecretSafeCredentialError(t, err, source, "unit-secret")
	})

	t.Run("preexisting artifact", func(t *testing.T) {
		destination := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(target, nil, 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(destination, claudeResumeCredentialFile)))

		err := copyClaudeResumeCredential(source, destination)
		require.ErrorIs(t, err, errClaudeResumeCredentialDestination)
		requireSecretSafeCredentialError(t, err, source, "unit-secret")
	})
}

func TestClaudeResumeCredentialIOFailures(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	data := []byte(`{"token":"unit-secret"}`)
	require.NoError(t, os.WriteFile(filepath.Join(source, claudeResumeCredentialFile), data, 0o600))

	originalLstat := resumeCredentialLstat
	originalOpenRoot := resumeCredentialOpenRoot
	originalRootLstat := resumeCredentialRootLstat
	originalRootOpen := resumeCredentialRootOpen
	originalFileStat := resumeCredentialFileStat
	originalReadAll := resumeCredentialReadAll
	originalFileClose := resumeCredentialFileClose
	originalRootStat := resumeCredentialRootStat
	originalRootChmod := resumeCredentialRootChmod
	originalRootOpenFile := resumeCredentialRootOpenFile
	originalRootRemove := resumeCredentialRootRemove
	originalFileWrite := resumeCredentialFileWrite
	originalFileChmod := resumeCredentialFileChmod
	reset := func() {
		resumeCredentialLstat = originalLstat
		resumeCredentialOpenRoot = originalOpenRoot
		resumeCredentialRootLstat = originalRootLstat
		resumeCredentialRootOpen = originalRootOpen
		resumeCredentialFileStat = originalFileStat
		resumeCredentialReadAll = originalReadAll
		resumeCredentialFileClose = originalFileClose
		resumeCredentialRootStat = originalRootStat
		resumeCredentialRootChmod = originalRootChmod
		resumeCredentialRootOpenFile = originalRootOpenFile
		resumeCredentialRootRemove = originalRootRemove
		resumeCredentialFileWrite = originalFileWrite
		resumeCredentialFileChmod = originalFileChmod
	}
	t.Cleanup(reset)

	readCases := []struct {
		name   string
		mutate func()
		want   error
	}{
		{
			name: "lstat",
			mutate: func() {
				resumeCredentialLstat = func(string) (os.FileInfo, error) {
					return nil, errors.New("lstat")
				}
			},
			want: errClaudeResumeCredentialSource,
		},
		{
			name: "open root",
			mutate: func() {
				resumeCredentialOpenRoot = func(string) (*os.Root, error) {
					return nil, errors.New("open root")
				}
			},
			want: errClaudeResumeCredentialSource,
		},
		{
			name: "root lstat",
			mutate: func() {
				resumeCredentialRootLstat = func(*os.Root, string) (os.FileInfo, error) {
					return nil, errors.New("root lstat")
				}
			},
			want: errClaudeResumeCredentialUnsafe,
		},
		{
			name: "root open",
			mutate: func() {
				resumeCredentialRootOpen = func(*os.Root, string) (*os.File, error) {
					return nil, errors.New("root open")
				}
			},
			want: errClaudeResumeCredentialUnreadable,
		},
		{
			name: "file stat",
			mutate: func() {
				resumeCredentialFileStat = func(*os.File) (os.FileInfo, error) {
					return nil, errors.New("file stat")
				}
			},
			want: errClaudeResumeCredentialUnsafe,
		},
		{
			name: "read",
			mutate: func() {
				resumeCredentialReadAll = func(io.Reader) ([]byte, error) {
					return []byte("partial"), errors.New("read")
				}
			},
			want: errClaudeResumeCredentialUnreadable,
		},
		{
			name: "close",
			mutate: func() {
				resumeCredentialFileClose = func(*os.File) error {
					return errors.New("close")
				}
			},
			want: errClaudeResumeCredentialUnreadable,
		},
		{
			name: "growth",
			mutate: func() {
				resumeCredentialReadAll = func(io.Reader) ([]byte, error) {
					return make([]byte, claudeResumeCredentialMaxBytes+1), nil
				}
			},
			want: errClaudeResumeCredentialOversized,
		},
	}
	for _, test := range readCases {
		t.Run("read "+test.name, func(t *testing.T) {
			reset()
			test.mutate()
			_, err := readClaudeResumeCredential(source)
			require.ErrorIs(t, err, test.want)
		})
	}

	writeCases := []struct {
		name   string
		mutate func()
	}{
		{
			name: "open root",
			mutate: func() {
				resumeCredentialOpenRoot = func(string) (*os.Root, error) {
					return nil, errors.New("open root")
				}
			},
		},
		{
			name: "root stat",
			mutate: func() {
				resumeCredentialRootStat = func(*os.Root, string) (os.FileInfo, error) {
					return nil, errors.New("root stat")
				}
			},
		},
		{
			name: "root chmod",
			mutate: func() {
				resumeCredentialRootChmod = func(*os.Root, string, os.FileMode) error {
					return errors.New("root chmod")
				}
			},
		},
		{
			name: "open file",
			mutate: func() {
				resumeCredentialRootOpenFile = func(*os.Root, string, int, os.FileMode) (*os.File, error) {
					return nil, errors.New("open file")
				}
			},
		},
		{
			name: "write",
			mutate: func() {
				resumeCredentialFileWrite = func(*os.File, []byte) (int, error) {
					return 0, errors.New("write")
				}
			},
		},
		{
			name: "short write",
			mutate: func() {
				resumeCredentialFileWrite = func(*os.File, []byte) (int, error) {
					return 0, nil
				}
			},
		},
		{
			name: "file chmod",
			mutate: func() {
				resumeCredentialFileChmod = func(*os.File, os.FileMode) error {
					return errors.New("file chmod")
				}
			},
		},
		{
			name: "file close",
			mutate: func() {
				resumeCredentialFileClose = func(*os.File) error {
					return errors.New("file close")
				}
			},
		},
	}
	for _, test := range writeCases {
		t.Run("write "+test.name, func(t *testing.T) {
			reset()
			test.mutate()
			require.ErrorIs(t, writeClaudeResumeCredential(destination, data), errClaudeResumeCredentialDestination)
			require.NoFileExists(t, filepath.Join(destination, claudeResumeCredentialFile))
		})
	}
}

func requireSecretSafeCredentialError(t *testing.T, err error, source string, secret string) {
	t.Helper()
	require.Error(t, err)
	require.NotContains(t, err.Error(), source)
	require.NotContains(t, err.Error(), claudeResumeCredentialFile)
	if secret != "" {
		require.NotContains(t, err.Error(), secret)
	}
	var pathErr *os.PathError
	require.False(t, errors.As(err, &pathErr))
}

func requireScratchDirEmpty(t *testing.T, scratch string) {
	t.Helper()
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Empty(t, entries)
}
