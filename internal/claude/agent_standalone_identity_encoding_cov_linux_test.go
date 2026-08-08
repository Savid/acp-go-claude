//go:build linux

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentStandaloneCovFaultEncoding makes every registry encoding fail. The
// records this file writes are structs of scalars and strings that always
// encode, so the seam is the only way to prove the callers treat an
// unencodable record as a refusal rather than as an empty payload.
func agentStandaloneCovFaultEncoding(t *testing.T, verdict error) {
	t.Helper()
	previous := agentStandaloneMarshal
	t.Cleanup(func() { agentStandaloneMarshal = previous })
	agentStandaloneMarshal = func(any) ([]byte, error) { return nil, verdict }
}

// agentStandaloneCovFaultCurrentDomain makes the nth reading of this process's
// authority domain fail. The reading is derived from procfs and the registry
// descriptor, neither of which a case can break without breaking everything
// else, so the seam is the only way to prove a claim aborts when it cannot say
// which domain it is in.
func agentStandaloneCovFaultCurrentDomain(t *testing.T, call int, verdict error) {
	t.Helper()
	previous := agentStandaloneCurrentDomain
	t.Cleanup(func() { agentStandaloneCurrentDomain = previous })
	hit := agentStandaloneCovNthCall(call)
	agentStandaloneCurrentDomain = func(directory *os.File) (agentAuthorityDomainRecord, error) {
		if hit() {
			return agentAuthorityDomainRecord{}, verdict
		}

		return previous(directory)
	}
}

// agentStandaloneCovBoundStateRoot binds a real protected state root, so a
// case can drive a claim through the state root revalidations that every
// completion performs.
func agentStandaloneCovBoundStateRoot(t *testing.T, uid, gid uint32) agentStandaloneStateRoot {
	t.Helper()
	bound, err := bindAgentStandaloneStateRoot(createAgentStandaloneProtectedStateRoot(t, uid, gid), uid, gid)
	require.NoError(t, err)

	return bound
}

// TestAgentStandaloneCovRegistryWritesRefuseAnUnencodableRecord proves every
// registry write refuses when its record cannot be encoded, and creates
// nothing. Publishing an empty payload would leave a permanent name whose
// content no later claim can attribute to any owner, domain or disposition.
func TestAgentStandaloneCovRegistryWritesRefuseAnUnencodableRecord(t *testing.T) {
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	owner := agentStandaloneCovStaticOwner(63301, 63302, "unencodable")

	t.Run("owner digest", func(t *testing.T) {
		agentStandaloneCovFaultEncoding(t, errors.New("injected owner digest encoding failure"))

		key, err := agentStandaloneOwnerDigest(owner)
		require.ErrorContains(t, err, "injected owner digest encoding failure")
		require.Empty(t, key)
	})

	t.Run("authority domain record", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		record, err := agentStandaloneCurrentDomain(directory)
		require.NoError(t, err)
		record.AuthorityID = "0123456789abcdef0123456789abcdef"
		wantErr := errors.New("injected domain record encoding failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t, replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, record), wantErr)
		entries, readErr := os.ReadDir(directory.Name())
		require.NoError(t, readErr)
		require.Empty(t, entries)
	})

	t.Run("owner binding", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		wantErr := errors.New("injected owner encoding failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t, createAgentStandaloneOwner(directory, owner, ownerUID, ownerGID), wantErr)
		entries, readErr := os.ReadDir(directory.Name())
		require.NoError(t, readErr)
		require.Empty(t, entries)
	})

	t.Run("ACTIVE marker", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		wantErr := errors.New("injected marker encoding failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t, publishAgentStandaloneActive(
			directory, owner.UID, owner.GID, ownerUID, ownerGID, "standalone:key",
			time.Now().Add(time.Second), nil, nil,
		), wantErr)
		entries, readErr := os.ReadDir(directory.Name())
		require.NoError(t, readErr)
		require.Empty(t, entries)
	})
}

// TestAgentStandaloneCovRetainedMarkerChecksRefuseAnUnderivableOwnerDigest
// proves every comparison of a retained marker against its owner refuses when
// the owner's owner digest cannot be derived. Comparing against an empty key
// would let a marker minted for a different owner tuple be accepted as this
// owner's own retained disposition.
func TestAgentStandaloneCovRetainedMarkerChecksRefuseAnUnderivableOwnerDigest(t *testing.T) {
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	owner := agentStandaloneCovOwner(63311, 63312, "session-derive", "/srv/claude/session-derive", 121, 122)

	t.Run("prior disposition", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		agentStandaloneCovWriteActiveMarker(t, directory, owner)
		wantErr := errors.New("injected disposition key failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t,
			validateAgentStandalonePriorDisposition(directory, owner, ownerUID, ownerGID),
			wantErr,
		)
	})

	t.Run("registry audit", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		agentStandaloneCovPermanentLock(t, directory, agentStandaloneCovOwnersLock)
		agentStandaloneCovPermanentLock(t, directory, "63311.lock")
		agentStandaloneCovWriteOwner(t, directory, owner)
		agentStandaloneCovWriteActiveMarker(t, directory, owner)
		wantErr := errors.New("injected audit key failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t, auditAgentStandaloneAuthorityRoot(
			directory, ownerUID, ownerGID, false, false, false, time.Now().Add(time.Second), nil, nil,
		), wantErr)
	})

	t.Run("same-boot rebind", func(t *testing.T) {
		directory, rebindUID, rebindGID, rebound := agentStandaloneCovRebindableFixture(t, 63313, 63314, "session-derive")
		wantErr := errors.New("injected rebind key failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		identity, err := validateAgentStandaloneSameBootRebind(
			directory, rebound, rebindUID, rebindGID, time.Now().Add(time.Second), nil, nil,
		)
		require.Nil(t, identity)
		require.ErrorIs(t, err, wantErr)
		contender, acquired, lockErr := tryAgentStandaloneNamedLock(
			directory, "63313.lock", false, rebindUID, rebindGID,
		)
		require.NoError(t, lockErr)
		require.True(t, acquired, "a refused rebind must release the UID lock it took")
		require.NoError(t, contender.Close())
	})

	t.Run("owner claim completion", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		claimant := owner
		claimant.StateRoot = agentStandaloneCovBoundStateRoot(t, claimant.UID, claimant.GID)
		agentStandaloneCovWriteOwner(t, directory, claimant)
		agentStandaloneCovNoVacancy(t, nil)
		wantErr := errors.New("injected completion key failure")
		agentStandaloneCovFaultEncoding(t, wantErr)

		require.ErrorIs(t, completeAgentStandaloneOwnerClaim(
			directory, claimant, ownerUID, ownerGID, true, time.Now().Add(time.Second), nil, nil,
		), wantErr)
		require.NoFileExists(t, filepath.Join(directory.Name(), "63311.quarantine"))
	})
}

// TestAgentStandaloneCovOwnerClaimRechecksCancellationBeforePublishing proves
// the claim re-checks cancellation after its final state root revalidation and
// publishes no ACTIVE marker when the claim was canceled in that window. The
// marker is what tells every later claim the identity is in use, so it must
// never outlive the acquisition that asked for it.
func TestAgentStandaloneCovOwnerClaimRechecksCancellationBeforePublishing(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	owner := agentStandaloneCovOwner(63321, 63322, "late-cancel", "/srv/claude/late-cancel", 1, 2)
	owner.StateRoot = agentStandaloneCovBoundStateRoot(t, owner.UID, owner.GID)
	agentStandaloneCovWriteOwner(t, directory, owner)
	agentStandaloneCovNoVacancy(t, nil)
	canceled := make(chan struct{})
	previous := agentStandaloneOpen
	binds := 0
	agentStandaloneOpen = func(path string, mode int, perm uint32) (int, error) {
		binds++
		if binds == 2 {
			close(canceled)
		}

		return previous(path, mode, perm)
	}
	t.Cleanup(func() { agentStandaloneOpen = previous })

	require.ErrorIs(t, completeAgentStandaloneOwnerClaim(
		directory, owner, ownerUID, ownerGID, true, time.Now().Add(time.Second), canceled, nil,
	), errAgentStandaloneCanceled)
	require.Equal(t, 2, binds, "both state root revalidations must have run")
	require.NoFileExists(t, filepath.Join(directory.Name(), "63321.quarantine"))
}

// TestAgentStandaloneCovStateRootBindRefusesWithoutAFilesystemRoot proves the
// state root walk refuses when it cannot open the filesystem root as a
// protected directory, instead of resolving the claimed path from wherever the
// process happens to be. The walk's guarantee is that every component from /
// down is root-owned protected storage, which only means anything if the walk
// really started at /.
func TestAgentStandaloneCovStateRootBindRefusesWithoutAFilesystemRoot(t *testing.T) {
	uid, gid := agentStandaloneTestAuthorityIDs()
	stateRoot := createAgentStandaloneProtectedStateRoot(t, uid, gid)
	previous := agentStandaloneOpen
	wantErr := errors.New("injected filesystem root failure")
	agentStandaloneOpen = func(string, int, uint32) (int, error) { return -1, wantErr }
	t.Cleanup(func() { agentStandaloneOpen = previous })

	bound, err := bindAgentStandaloneStateRoot(stateRoot, uid, gid)
	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, "open filesystem root for standalone state root")
	require.Equal(t, agentStandaloneStateRoot{}, bound)
}

// TestAgentStandaloneCovAuditRefusesARegistryItCannotName proves the audit
// refuses when the registry descriptor no longer resolves to a live directory.
// The resolved path is what decides whether a standalone owner's state root
// sits inside the authority registry, so an audit that ran without it would
// accept an owner whose state root it never checked.
func TestAgentStandaloneCovAuditRefusesARegistryItCannotName(t *testing.T) {
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	directory := agentStandaloneCovRemovedDirectory(t)
	previous := agentStandaloneOpenat
	agentStandaloneOpenat = func(dirfd int, path string, flags int, mode uint32) (int, error) {
		if path == "." {
			return unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		}

		return previous(dirfd, path, flags, mode)
	}
	t.Cleanup(func() { agentStandaloneOpenat = previous })

	require.ErrorContains(t, auditAgentStandaloneAuthorityRoot(
		directory, ownerUID, ownerGID, false, false, false, time.Now().Add(time.Second), nil, nil,
	), "deleted directory")
}

// TestAgentStandaloneCovDomainRecordRefusesADifferentPublishedRecord proves
// the authority record publication compares what it reads back against the
// bytes it wrote and refuses a well-formed record that is not its own. Parsing
// alone is not enough: a peer's valid record must never be mistaken for this
// claim's authority.
func TestAgentStandaloneCovDomainRecordRefusesADifferentPublishedRecord(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	record, err := agentStandaloneCurrentDomain(directory)
	require.NoError(t, err)
	record.AuthorityID = "0123456789abcdef0123456789abcdef"
	previous := agentStandaloneCloseTemporary
	agentStandaloneCloseTemporary = func(file *os.File) error {
		require.NoError(t, file.Close())
		path := filepath.Join(directory.Name(), file.Name())
		payload, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		rewritten := strings.Replace(
			string(payload), record.AuthorityID, "fedcba9876543210fedcba9876543210", 1,
		)
		require.NotEqual(t, string(payload), rewritten)

		return os.WriteFile(path, []byte(rewritten), 0o600)
	}
	t.Cleanup(func() { agentStandaloneCloseTemporary = previous })

	err = replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, record)
	require.ErrorContains(t, err, "published agent authority record payload changed")
	published, loadErr := loadAgentAuthorityDomainRecord(directory, ownerUID, ownerGID)
	require.NoError(t, loadErr)
	require.Equal(t, "fedcba9876543210fedcba9876543210", published.AuthorityID,
		"the refusal must be attributable to the record that actually landed")
}

// TestAgentStandaloneCovProbeRefusesWhenItCannotReleaseItsContender proves the
// durability probe refuses when the second descriptor it opened to prove flock
// exclusion cannot be closed. A probe that returned success while leaking that
// descriptor would leave an open write descriptor on the registry for the life
// of the process.
func TestAgentStandaloneCovProbeRefusesWhenItCannotReleaseItsContender(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	agentStandaloneCovRestoreSyscallSeams(t)
	previousOpen := agentStandaloneOpenat
	opens := 0
	agentStandaloneOpenat = func(dirfd int, path string, flags int, mode uint32) (int, error) {
		opens++
		if opens == 2 {
			return 0x7ffe, nil
		}

		return previousOpen(dirfd, path, flags, mode)
	}
	previousFlock := agentStandaloneFlock
	locks := 0
	agentStandaloneFlock = func(fd, how int) error {
		locks++
		if locks == 2 {
			return unix.EWOULDBLOCK
		}

		return previousFlock(fd, how)
	}

	require.ErrorIs(t, probeAgentStandaloneFilesystem(directory, true), unix.EBADF)
	require.Equal(t, 2, opens)
	entries, err := os.ReadDir(directory.Name())
	require.NoError(t, err)
	require.Empty(t, entries, "a refused probe must leave no temporary behind")
}

// TestAgentStandaloneCovDomainClaimAbortsWhenItsOwnDomainIsUnreadable proves
// every point at which a claim reads the domain it is actually running in
// treats an unreadable answer as a refusal. The comparison against the
// published record is the only thing that distinguishes "this registry is
// mine" from "this registry belongs to another boot or namespace", so a claim
// that proceeded without it could adopt a foreign authority or overwrite a
// live one.
func TestAgentStandaloneCovDomainClaimAbortsWhenItsOwnDomainIsUnreadable(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		call  int
		setup func(t *testing.T) (*os.File, uint32, uint32, agentStandaloneOwner)
	}{
		{
			name: "under the shared lease",
			call: 1,
			setup: func(t *testing.T) (*os.File, uint32, uint32, agentStandaloneOwner) {
				t.Helper()

				return createAgentStandaloneMatchingDomainFixture(t)
			},
		},
		{
			name: "after upgrading to the exclusive lease",
			call: 2,
			setup: func(t *testing.T) (*os.File, uint32, uint32, agentStandaloneOwner) {
				t.Helper()
				directory, ownerUID, ownerGID, want := createAgentStandaloneMatchingDomainFixture(t)
				agentStandaloneCovWriteRegistryFile(
					t, directory, "domain.json.next-"+agentStandaloneCovSuffix, "partial",
				)

				return directory, ownerUID, ownerGID, want
			},
		},
		{
			name: "under the exclusive lease of a foreign record",
			call: 2,
			setup: func(t *testing.T) (*os.File, uint32, uint32, agentStandaloneOwner) {
				t.Helper()
				directory, ownerUID, ownerGID := agentStandaloneCovDivergentDomainFixture(t)

				return directory, ownerUID, ownerGID, agentStandaloneCovStaticOwner(63331, 63332, "foreign")
			},
		},
		{
			name: "before minting the first record",
			call: 1,
			setup: func(t *testing.T) (*os.File, uint32, uint32, agentStandaloneOwner) {
				t.Helper()
				directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)

				return directory, ownerUID, ownerGID, agentStandaloneCovStaticOwner(63333, 63334, "pristine")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory, ownerUID, ownerGID, want := testCase.setup(t)
			before, readErr := os.ReadFile(filepath.Join(directory.Name(), agentStandaloneCovDomainRecord))
			wantErr := errors.New("injected current domain failure")
			agentStandaloneCovFaultCurrentDomain(t, testCase.call, wantErr)

			lease, err := acquireAgentStandaloneDomain(
				directory, want, ownerUID, ownerGID, true, time.Now().Add(time.Second), nil, nil,
			)
			require.Nil(t, lease)
			require.ErrorIs(t, err, wantErr)
			after, afterErr := os.ReadFile(filepath.Join(directory.Name(), agentStandaloneCovDomainRecord))
			require.Equal(t, readErr == nil, afterErr == nil, "the published record must not appear or vanish")
			require.Equal(t, before, after, "a refused claim must not change the published record")
		})
	}
}

// TestAgentStandaloneCovFirstClaimWaitsOutAMarkerTemporaryAPeerTook proves the
// first-ever claim, which requires an otherwise empty authority root, treats a
// marker temporary whose UID lock a peer took after the registry was listed as
// a reason to wait and retry rather than as a reason to delete it or to mint
// authority over it. It also proves the claim gives up when the retry outlives
// the claim budget.
func TestAgentStandaloneCovFirstClaimWaitsOutAMarkerTemporaryAPeerTook(t *testing.T) {
	const uid = 63351
	want := agentStandaloneCovStaticOwner(uid, 63352, "peer-marker-temp")
	lockName := strconv.FormatUint(uid, 10) + ".lock"
	temporary := strconv.FormatUint(uid, 10) + ".quarantine.next-" + agentStandaloneCovSuffix

	stage := func(t *testing.T) (*os.File, uint32, uint32, string) {
		t.Helper()
		directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)
		path := agentStandaloneCovWriteRegistryFile(t, directory, temporary, "partial")
		restoreAgentStandalonePermanentLockSeams(t)
		previous := agentStandaloneLockOpenat
		taken := false
		agentStandaloneLockOpenat = func(dirfd int, name string, flags int, mode uint32) (int, error) {
			if name == lockName && !taken {
				taken = true
				held := createAgentStandaloneTestLock(t, directory, lockName, ownerUID, ownerGID)
				require.NoError(t, unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB))
			}

			return previous(dirfd, name, flags, mode)
		}

		return directory, ownerUID, ownerGID, path
	}

	t.Run("retries while the budget lasts", func(t *testing.T) {
		directory, ownerUID, ownerGID, path := stage(t)

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, true, time.Now().Add(500*time.Millisecond), nil, nil,
		)
		require.Nil(t, lease)
		require.ErrorContains(t, err, "record is missing but root contains prior lock",
			"the retry must re-list the registry and see the peer's lock",
		)
		require.FileExists(t, path, "a temporary with a live holder must never be removed")
		require.NoFileExists(t, filepath.Join(directory.Name(), agentStandaloneCovDomainRecord))
	})

	t.Run("gives up when the budget is spent", func(t *testing.T) {
		directory, ownerUID, ownerGID, path := stage(t)

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, true, time.Now().Add(time.Millisecond), nil, nil,
		)
		require.Nil(t, lease)
		require.ErrorContains(t, err, "exceeded 30 seconds")
		require.FileExists(t, path)
		require.NoFileExists(t, filepath.Join(directory.Name(), agentStandaloneCovDomainRecord))
	})
}
