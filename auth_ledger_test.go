package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func disconnectParams(sessionID acp.SessionId, generation int64) map[string]any {
	return map[string]any{
		"sessionId":         string(sessionID),
		"providerId":        authProviderID,
		"connectionId":      testConnectionID,
		"bindingGeneration": generation,
	}
}

func newDisconnectBroker(t *testing.T) (*providerAuth, acp.SessionId) {
	t.Helper()

	home := t.TempDir()

	return newAuthBroker(t, WithHome(home), WithProviderAuthDirectHome(home))
}

func TestAuthLedgerRootValidation(t *testing.T) {
	require.False(t, authLedgerRootConfigured(Options{}))
	require.True(t, authLedgerRootConfigured(Options{ProviderAuthRoot: "/tmp"}))

	_, err := newAuthLedger(Options{ProviderAuthRoot: "relative"})
	require.Error(t, err)

	// The configured root is a pre-existing directory whose mode the operator
	// chose; the ledger under it is only as private as the directory holding
	// it, so the root is restricted too rather than only its leaf.
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Mkdir(root, 0o755))

	ledger, err := newAuthLedger(Options{ProviderAuthRoot: root, Home: "/home"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, authLedgerVendorDir, authLedgerHomeKey("/home"), authLedgerLeafDir), ledger.dir)

	rootInfo, err := os.Stat(root)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(authLedgerDirMode), rootInfo.Mode().Perm())

	info, err := os.Stat(ledger.dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(authLedgerDirMode), info.Mode().Perm())

	// Two agents pointed at different config dirs never alias.
	require.NotEqual(t, authLedgerHomeKey("/home"), authLedgerHomeKey("/other"))
}

func TestNewAuthLedgerFailsClosedOnEveryUnusableRoot(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		name    string
		arrange func(t *testing.T)
	}{
		{"the configured root cannot be created", func(t *testing.T) {
			t.Helper()

			original := ledgerMkdirAll
			ledgerMkdirAll = func(string, fs.FileMode) error { return errTestRandom }

			t.Cleanup(func() { ledgerMkdirAll = original })
		}},
		{"the configured root cannot be restricted", func(t *testing.T) {
			t.Helper()

			original := ledgerChmod
			ledgerChmod = func(string, fs.FileMode) error { return errTestRandom }

			t.Cleanup(func() { ledgerChmod = original })
		}},
		{"the ledger directory cannot be created", func(t *testing.T) {
			t.Helper()

			original := ledgerMkdirAll
			ledgerMkdirAll = func(path string, mode fs.FileMode) error {
				if path == root {
					return original(path, mode)
				}

				return errTestRandom
			}

			t.Cleanup(func() { ledgerMkdirAll = original })
		}},
		{"the ledger directory cannot be restricted", func(t *testing.T) {
			t.Helper()

			original := ledgerChmod
			ledgerChmod = func(path string, mode fs.FileMode) error {
				if path == root {
					return original(path, mode)
				}

				return errTestRandom
			}

			t.Cleanup(func() { ledgerChmod = original })
		}},
		{"the directory cannot be inspected", func(t *testing.T) {
			t.Helper()

			original := ledgerStat
			ledgerStat = func(string) (fs.FileInfo, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerStat = original })
		}},
		{"the path is not a directory", func(t *testing.T) {
			t.Helper()

			original := ledgerStat
			ledgerStat = func(name string) (fs.FileInfo, error) {
				file := filepath.Join(t.TempDir(), "file")
				require.NoError(t, os.WriteFile(file, nil, 0o600))

				return os.Stat(file)
			}

			t.Cleanup(func() { ledgerStat = original })
		}},
		{"the directory is not writable", func(t *testing.T) {
			t.Helper()

			original := ledgerCreateTemp
			ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerCreateTemp = original })
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.arrange(t)

			_, err := newAuthLedger(Options{ProviderAuthRoot: root})
			require.Error(t, err)
		})
	}
}

func TestAuthLedgerWriteIsAtomicAndValuesFree(t *testing.T) {
	broker, _ := newAuthBroker(t)

	record := authLedgerRecord{
		ProviderID:         authProviderID,
		ConnectionID:       testConnectionID,
		Revision:           2,
		BindingGeneration:  3,
		FlowID:             "flow-1",
		AuthorizeRequestID: testRequestID,
		State:              authLedgerConfirmed,
		CreatedAt:          1,
		UpdatedAt:          2,
	}
	require.NoError(t, broker.ledger.write(record))

	read, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, record, read)

	contents, err := os.ReadFile(broker.ledger.path(authProviderID))
	require.NoError(t, err)

	// The record's whole content is slot identity and provenance.
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(contents, &decoded))
	require.ElementsMatch(t, []string{
		"providerId", "connectionId", "revision", "bindingGeneration",
		"flowId", "authorizeRequestId", "state", "createdAt", "updatedAt",
	}, keysOf(decoded))

	info, err := os.Stat(broker.ledger.path(authProviderID))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(authLedgerFileMode), info.Mode().Perm())

	records, err := broker.ledger.list()
	require.NoError(t, err)
	require.Equal(t, []authLedgerRecord{record}, records)
}

func keysOf(values map[string]any) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	return names
}

func TestAuthLedgerReadAndListFailures(t *testing.T) {
	broker, _ := newAuthBroker(t)

	_, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, os.WriteFile(broker.ledger.path(authProviderID), []byte("not json"), 0o600))

	_, _, err = broker.ledger.read(authProviderID)
	require.Error(t, err)

	_, err = broker.ledger.list()
	require.Error(t, err)

	require.NoError(t, os.Remove(broker.ledger.path(authProviderID)))
	require.NoError(t, os.Mkdir(filepath.Join(broker.ledger.dir, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(broker.ledger.dir, "other.txt"), []byte("x"), 0o600))

	records, err := broker.ledger.list()
	require.NoError(t, err)
	require.Empty(t, records)

	readOriginal := ledgerReadFile

	ledgerReadFile = func(string) ([]byte, error) { return nil, errTestRandom }

	_, _, err = broker.ledger.read(authProviderID)
	require.Error(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(broker.ledger.dir, "entry.json"), []byte("{}"), 0o600))

	_, err = broker.ledger.list()
	require.Error(t, err)

	ledgerReadFile = readOriginal

	dirOriginal := ledgerReadDir

	ledgerReadDir = func(string) ([]os.DirEntry, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerReadDir = dirOriginal })

	_, err = broker.ledger.list()
	require.Error(t, err)
}

func TestAuthLedgerWriteFailures(t *testing.T) {
	broker, _ := newAuthBroker(t)
	record := authLedgerRecord{ProviderID: authProviderID}

	cases := []struct {
		name    string
		arrange func(t *testing.T)
	}{
		{"the record cannot be encoded", func(t *testing.T) {
			t.Helper()

			original := ledgerMarshal
			ledgerMarshal = func(any) ([]byte, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerMarshal = original })
		}},
		{"the temporary file cannot be created", func(t *testing.T) {
			t.Helper()

			original := ledgerCreateTemp
			ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerCreateTemp = original })
		}},
		{"the rename cannot commit", func(t *testing.T) {
			t.Helper()

			original := ledgerRename
			ledgerRename = func(string, string) error { return errTestRandom }

			t.Cleanup(func() { ledgerRename = original })
		}},
		{"the directory cannot be synced", func(t *testing.T) {
			t.Helper()

			original := ledgerOpen
			ledgerOpen = func(string) (*os.File, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerOpen = original })
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.arrange(t)
			require.Error(t, broker.ledger.write(record))
		})
	}
}

// failingLedgerFile drives each step of the atomic write independently.
type failingLedgerFile struct {
	name      string
	writeErr  error
	chmodErr  error
	syncErr   error
	closeErr  error
	closeCall int
}

func (f *failingLedgerFile) Name() string { return f.name }

func (f *failingLedgerFile) Write([]byte) (int, error) { return 0, f.writeErr }

func (f *failingLedgerFile) Chmod(fs.FileMode) error { return f.chmodErr }

func (f *failingLedgerFile) Sync() error { return f.syncErr }

func (f *failingLedgerFile) Close() error {
	f.closeCall++

	return f.closeErr
}

func TestWriteLedgerFileFailsAtEveryStep(t *testing.T) {
	for _, file := range []*failingLedgerFile{
		{writeErr: errTestRandom},
		{chmodErr: errTestRandom},
		{syncErr: errTestRandom},
		{closeErr: errTestRandom},
	} {
		require.Error(t, writeLedgerFile(file, []byte("{}")))
		require.Equal(t, 1, file.closeCall)
	}
}

func TestAuthProofSourceIsTotal(t *testing.T) {
	require.Equal(t, authProofConfirmedPresent, authProofSource(authLedgerConfirmed, true))
	require.Equal(t, authProofConfirmedAbsent, authProofSource(authLedgerConfirmed, false))
	require.Equal(t, authProofNotConfirmed, authProofSource(authLedgerIntent, true))
	require.Equal(t, authProofNotConfirmed, authProofSource(authLedgerIntent, false))
	require.Equal(t, authProofNotConfirmed, authProofSource(authLedgerRemoved, true))
}

func TestInventoryReportsResidenceFromTheLedgerAndAProbe(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	// With no ledger entry there is nothing to report and no native read.
	result, err := broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)
	require.Equal(t, authInventoryResult{Entries: []authInventoryEntry{}}, result)
	require.Zero(t, seams.statusCalls)

	flow := startAuthFlow(t, broker, sessionID)

	// A write-ahead intent can never be confirmed, however plainly the slot is
	// occupied.
	result, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)

	entries, ok := result.(authInventoryResult)
	require.True(t, ok)
	require.Len(t, entries.Entries, 1)
	require.Equal(t, authProofNotConfirmed, entries.Entries[0].ProofSource)
	require.Equal(t, testConnectionID, entries.Entries[0].ConnectionID)

	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	require.NoError(t, err)

	result, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)

	confirmed, ok := result.(authInventoryResult)
	require.True(t, ok)
	require.Equal(t, authProofConfirmedPresent, confirmed.Entries[0].ProofSource)

	seams.account = claude.AuthAccount{}

	result, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)

	absent, ok := result.(authInventoryResult)
	require.True(t, ok)
	require.Equal(t, authProofConfirmedAbsent, absent.Entries[0].ProofSource)
}

func TestInventoryFailures(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	_, err := broker.inventory(t.Context(), json.RawMessage(`{"nope":1}`))
	requireInvalidAuthField(t, err, "nope")

	_, err = broker.inventory(t.Context(), authParams(t, map[string]any{}))
	requireInvalidAuthField(t, err, "sessionId")

	_, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": "missing"}))
	requireInvalidAuthField(t, err, "sessionId")

	original := ledgerReadDir

	ledgerReadDir = func(string) ([]os.DirEntry, error) { return nil, errTestRandom }

	_, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	requireAuthFailed(t, err, authCauseHarvestFailed)

	ledgerReadDir = original

	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: authProviderID, State: authLedgerConfirmed}))

	seams.statusErr = errTestRandom

	_, err = broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestInventorySkipsRemovedEntries(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: authProviderID, State: authLedgerRemoved}))

	result, err := broker.inventory(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)
	require.Equal(t, authInventoryResult{Entries: []authInventoryEntry{}}, result)
	require.Zero(t, seams.statusCalls)
}

func TestDisconnectClearsTheFencedSlotAndVerifiesAbsence(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newDisconnectBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	require.NoError(t, err)

	seams.account = claude.AuthAccount{}

	result, err := broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
	require.NoError(t, err)
	require.Equal(t, struct{}{}, result)
	require.Equal(t, 1, seams.logoutCalls)
	require.Equal(t, 1, seams.removeCalls)
	require.Equal(t, "canary-user", seams.removedUser)
	require.True(t, strings.HasSuffix(seams.removedDir, filepath.Base(broker.agent.options.Home)))

	record, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerRemoved, record.State)
	require.Equal(t, int64(2), record.BindingGeneration)
}

func TestDisconnectFencesEveryMismatch(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newDisconnectBroker(t)

	_, err := broker.disconnect(t.Context(), json.RawMessage(`{"nope":1}`))
	requireInvalidAuthField(t, err, "nope")

	for _, missing := range []string{"sessionId", "providerId", "connectionId", "bindingGeneration"} {
		params := disconnectParams(sessionID, 1)
		delete(params, missing)

		_, missingErr := broker.disconnect(t.Context(), authParams(t, params))
		requireInvalidAuthField(t, missingErr, missing)
	}

	_, err = broker.disconnect(t.Context(), authParams(t, disconnectParams("missing", 1)))
	requireInvalidAuthField(t, err, "sessionId")

	// No ledger entry at all.
	_, err = broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
	requireAuthFailed(t, err, authCausePolicy)

	require.NoError(t, broker.ledger.write(authLedgerRecord{
		ProviderID:        authProviderID,
		ConnectionID:      testConnectionID,
		BindingGeneration: 1,
		State:             authLedgerConfirmed,
	}))

	// A differently fenced generation is refused before anything is touched.
	_, err = broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 99)))
	requireAuthFailed(t, err, authCausePolicy)

	wrongConnection := disconnectParams(sessionID, 1)
	wrongConnection["connectionId"] = "other"

	_, err = broker.disconnect(t.Context(), authParams(t, wrongConnection))
	requireAuthFailed(t, err, authCausePolicy)
}

func TestDisconnectNativeFailures(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*testing.T, *authTestSeams)
		cause   string
	}{
		{"the ledger cannot be read", func(t *testing.T, _ *authTestSeams) {
			t.Helper()

			original := ledgerReadFile
			ledgerReadFile = func(string) ([]byte, error) { return nil, errTestRandom }

			t.Cleanup(func() { ledgerReadFile = original })
		}, authCauseHarvestFailed},
		{"the bumped generation cannot be persisted", func(t *testing.T, _ *authTestSeams) {
			t.Helper()

			original := ledgerRename
			ledgerRename = func(string, string) error { return errTestRandom }

			t.Cleanup(func() { ledgerRename = original })
		}, authCauseProcess},
		{"native logout failed", func(t *testing.T, seams *authTestSeams) {
			t.Helper()

			seams.logoutErr = errTestRandom
		}, authCauseProcess},
		{"the keystore item could not be removed", func(t *testing.T, seams *authTestSeams) {
			t.Helper()

			seams.removeErr = errTestRandom
		}, authCauseTransport},
		{"the credential is still resident", func(t *testing.T, seams *authTestSeams) {
			t.Helper()

			seams.account = claude.AuthAccount{LoggedIn: true}
		}, authCauseHarvestFailed},
		{
			// A verification that never ran is not a verification that found
			// the slot empty or occupied, and the deadline that stopped it is
			// the answer rather than a harvest verdict.
			"the absence check hit the wrapper deadline",
			func(t *testing.T, seams *authTestSeams) {
				t.Helper()

				seams.statusErr = context.DeadlineExceeded
			},
			authCauseTimeout,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seams := newAuthSeams(t)
			seams.account = claude.AuthAccount{}

			broker, sessionID := newDisconnectBroker(t)
			require.NoError(t, broker.ledger.write(authLedgerRecord{
				ProviderID:        authProviderID,
				ConnectionID:      testConnectionID,
				BindingGeneration: 1,
				State:             authLedgerConfirmed,
			}))

			testCase.arrange(t, seams)

			_, err := broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
			requireAuthFailed(t, err, testCase.cause)
		})
	}
}

func TestDisconnectFailsWhenTheRemovalCannotBeRecorded(t *testing.T) {
	seams := newAuthSeams(t)
	seams.account = claude.AuthAccount{}

	broker, sessionID := newDisconnectBroker(t)
	require.NoError(t, broker.ledger.write(authLedgerRecord{
		ProviderID:        authProviderID,
		ConnectionID:      testConnectionID,
		BindingGeneration: 1,
		State:             authLedgerConfirmed,
	}))

	writes := 0
	original := ledgerRename

	ledgerRename = func(oldPath, newPath string) error {
		writes++
		if writes > 1 {
			return errTestRandom
		}

		return original(oldPath, newPath)
	}

	t.Cleanup(func() { ledgerRename = original })

	_, err := broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestRecordAuthorizeIntentCarriesTheEarlierBinding(t *testing.T) {
	broker, _ := newAuthBroker(t)

	require.NoError(t, broker.ledger.write(authLedgerRecord{
		ProviderID:        authProviderID,
		Revision:          4,
		BindingGeneration: 7,
		CreatedAt:         11,
		State:             authLedgerConfirmed,
	}))

	record, err := broker.recordAuthorizeIntent(authorizeRequest{
		providerID:         authProviderID,
		connectionID:       testConnectionID,
		authorizeRequestID: testRequestID,
	}, "flow-2")
	require.NoError(t, err)
	require.Equal(t, int64(5), record.Revision)
	require.Equal(t, int64(7), record.BindingGeneration)
	require.Equal(t, int64(11), record.CreatedAt)
	require.Equal(t, authLedgerIntent, record.State)

	original := ledgerMarshal

	ledgerMarshal = func(any) ([]byte, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerMarshal = original })

	_, err = broker.recordAuthorizeIntent(authorizeRequest{providerID: authProviderID}, "flow-3")
	requireAuthFailed(t, err, authCauseProcess)
}

func TestLedgerReadIgnoresAMissingEntry(t *testing.T) {
	broker, _ := newAuthBroker(t)

	_, ok, err := broker.ledger.read("absent")
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, errors.Is(fs.ErrNotExist, fs.ErrNotExist))
}

func TestLedgerWriteRemovesTheTemporaryFileOnFailure(t *testing.T) {
	broker, _ := newAuthBroker(t)

	original := ledgerCreateTemp

	ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) {
		file, err := os.CreateTemp(dir, pattern)
		require.NoError(t, err)

		return &failingLedgerFile{name: file.Name(), writeErr: errTestRandom}, nil
	}

	t.Cleanup(func() { ledgerCreateTemp = original })

	require.Error(t, broker.ledger.write(authLedgerRecord{ProviderID: authProviderID}))

	entries, err := os.ReadDir(broker.ledger.dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestLedgerListRejectsAMalformedEntry(t *testing.T) {
	broker, _ := newAuthBroker(t)

	require.NoError(t, os.WriteFile(filepath.Join(broker.ledger.dir, "bad.json"), []byte("not json"), 0o600))

	_, err := broker.ledger.list()
	require.Error(t, err)
}

func TestLedgerListIsOrderedByProviderSlot(t *testing.T) {
	broker, _ := newAuthBroker(t)

	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "zeta", State: authLedgerConfirmed}))
	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "alpha", State: authLedgerConfirmed}))

	records, err := broker.ledger.list()
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "alpha", records[0].ProviderID)
	require.Equal(t, "zeta", records[1].ProviderID)
}
