//go:build linux

package claude

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentStandaloneCovRestorePublicationSeams restores every seam a publication
// case may fault, so a fault can never leak into the next case.
func agentStandaloneCovRestorePublicationSeams(t *testing.T) {
	t.Helper()
	random := agentStandaloneRandom
	marshal := agentStandaloneMarshal
	fchown := agentStandaloneRecordFchown
	fchmod := agentStandaloneRecordFchmod
	write := agentStandaloneRecordWrite
	sync := agentStandaloneRecordSync
	fstat := agentStandaloneFstat
	unlinkat := agentStandaloneUnlinkat
	readAll := agentStandaloneReadAll
	closeTemporary := agentStandaloneCloseTemporary
	t.Cleanup(func() {
		agentStandaloneRandom = random
		agentStandaloneMarshal = marshal
		agentStandaloneRecordFchown = fchown
		agentStandaloneRecordFchmod = fchmod
		agentStandaloneRecordWrite = write
		agentStandaloneRecordSync = sync
		agentStandaloneFstat = fstat
		agentStandaloneUnlinkat = unlinkat
		agentStandaloneReadAll = readAll
		agentStandaloneCloseTemporary = closeTemporary
	})
}

// agentStandaloneCovPublication is one of the three durable registry writes.
// All three follow the same protocol — create a private temporary, give it the
// registry owner's metadata, write it, prove its inode, then rename it over the
// permanent name — so every case below states one property once and holds all
// three to it.
type agentStandaloneCovPublication struct {
	name        string
	published   string
	swappedWant string
	appendWant  string
	publish     func(target *os.File) error
}

// agentStandaloneCovPublications binds the three publication entry points to a
// live registry descriptor. Everything a case must compute before it faults a
// seam — the domain record and the owner's session key — is computed here.
func agentStandaloneCovPublications(t *testing.T, directory *os.File) []agentStandaloneCovPublication {
	t.Helper()
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	owner := agentStandaloneCovOwner(62701, 62702, "cov-publication", "/srv/claude/cov-publication", 91, 92)
	record, err := currentAgentAuthorityDomain(directory)
	require.NoError(t, err)
	record.AuthorityID = "0123456789abcdef0123456789abcdef"
	key, err := agentStandaloneSessionKey(owner)
	require.NoError(t, err)

	return []agentStandaloneCovPublication{
		{
			name:        "authority domain record",
			published:   "domain.json",
			swappedWant: "published agent authority record is not the temporary inode",
			appendWant:  "json contains multiple values",
			publish: func(target *os.File) error {
				return replaceAgentStandaloneDomainRecord(target, ownerUID, ownerGID, record)
			},
		},
		{
			name:        "standalone owner binding",
			published:   "62701.owner",
			swappedWant: "published standalone owner is not its trusted named inode",
			appendWant:  "published standalone owner payload changed",
			publish: func(target *os.File) error {
				return createAgentStandaloneOwner(target, owner, ownerUID, ownerGID)
			},
		},
		{
			name:        "ACTIVE identity marker",
			published:   "62701.quarantine",
			swappedWant: "published agent identity marker is not the temporary inode",
			appendWant:  "published agent identity marker payload changed",
			publish: func(target *os.File) error {
				return publishAgentStandaloneActive(
					target, owner.UID, owner.GID, ownerUID, ownerGID, key,
					time.Now().Add(time.Second), nil, nil,
				)
			},
		},
	}
}

// agentStandaloneCovTemporaries lists the private temporaries a publication
// left behind in the registry.
func agentStandaloneCovTemporaries(t *testing.T, directory *os.File) []string {
	t.Helper()
	entries, err := os.ReadDir(directory.Name())
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".next-") {
			names = append(names, entry.Name())
		}
	}

	return names
}

// TestAgentStandaloneCovPublicationRefusesADetachedRegistryDescriptor proves
// each durable registry write refuses when the kernel has stopped answering for
// the registry descriptor it was handed, instead of creating its temporary
// somewhere else. The descriptor is the only thing that names the registry, so
// a write that proceeded without it would publish authority state into a
// directory nobody is holding.
func TestAgentStandaloneCovPublicationRefusesADetachedRegistryDescriptor(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	for _, publication := range agentStandaloneCovPublications(t, directory) {
		t.Run(publication.name, func(t *testing.T) {
			detached, err := os.Open(directory.Name())
			require.NoError(t, err)
			require.NoError(t, detached.Close())

			require.ErrorIs(t, publication.publish(detached), unix.EBADF)
			require.NoFileExists(t, filepath.Join(directory.Name(), publication.published))
			require.Empty(t, agentStandaloneCovTemporaries(t, directory))
		})
	}
}

// TestAgentStandaloneCovPublicationFaultsLeaveNothingBehind proves every step
// of the publication protocol fails closed: when randomness, encoding, the
// temporary's ownership, its mode, its bytes, its durability or its inode proof
// is refused, the permanent name is never created and the private temporary is
// removed. A temporary that survived a failed write would be adopted by the
// next claim's registry audit as unaccountable state.
func TestAgentStandaloneCovPublicationFaultsLeaveNothingBehind(t *testing.T) {
	for _, fault := range []struct {
		name    string
		install func(err error)
	}{
		{
			name: "randomness",
			install: func(err error) {
				agentStandaloneRandom = func([]byte) (int, error) { return 0, err }
			},
		},
		{
			name: "encoding",
			install: func(err error) {
				agentStandaloneMarshal = func(any) ([]byte, error) { return nil, err }
			},
		},
		{
			name: "temporary ownership",
			install: func(err error) {
				agentStandaloneRecordFchown = func(int, int, int) error { return err }
			},
		},
		{
			name: "temporary mode",
			install: func(err error) {
				agentStandaloneRecordFchmod = func(int, uint32) error { return err }
			},
		},
		{
			name: "payload write",
			install: func(err error) {
				agentStandaloneRecordWrite = func(*os.File, []byte) (int, error) { return 0, err }
			},
		},
		{
			name: "payload durability",
			install: func(err error) {
				agentStandaloneRecordSync = func(*os.File) error { return err }
			},
		},
		{
			name: "temporary inode proof",
			install: func(err error) {
				agentStandaloneFstat = func(int, *unix.Stat_t) error { return err }
			},
		},
	} {
		t.Run(fault.name, func(t *testing.T) {
			directory := openAgentStandaloneTestDirectory(t)
			for _, publication := range agentStandaloneCovPublications(t, directory) {
				t.Run(publication.name, func(t *testing.T) {
					agentStandaloneCovRestorePublicationSeams(t)
					wantErr := errors.New("injected " + fault.name + " failure")
					fault.install(wantErr)

					require.ErrorIs(t, publication.publish(directory), wantErr)
					require.NoFileExists(t, filepath.Join(directory.Name(), publication.published))
					require.Empty(t, agentStandaloneCovTemporaries(t, directory))
				})
			}
		})
	}
}

// TestAgentStandaloneCovPublicationReportsAFailedTemporaryCleanup proves that
// when the registry descriptor is dropped after the temporary was proven but
// before it was renamed, the publication refuses and reports that its temporary
// could not be removed, leaving the temporary visible rather than claiming a
// clean failure. The next claim has to see that residue: a silent loss would
// leave an unaccounted temporary in the authority root forever.
func TestAgentStandaloneCovPublicationReportsAFailedTemporaryCleanup(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	for _, publication := range agentStandaloneCovPublications(t, directory) {
		t.Run(publication.name, func(t *testing.T) {
			agentStandaloneCovRestorePublicationSeams(t)
			target, err := os.Open(directory.Name())
			require.NoError(t, err)
			closed := false
			t.Cleanup(func() {
				if !closed {
					require.NoError(t, target.Close())
				}
			})
			agentStandaloneCloseTemporary = func(file *os.File) error {
				require.NoError(t, file.Close())
				closed = true

				return target.Close()
			}

			require.ErrorIs(t, publication.publish(target), unix.EBADF)
			require.NoFileExists(t, filepath.Join(directory.Name(), publication.published))
			temporaries := agentStandaloneCovTemporaries(t, directory)
			require.Len(t, temporaries, 1, "the unremovable temporary must stay visible")
			require.NoError(t, os.Remove(filepath.Join(directory.Name(), temporaries[0])))
		})
	}
}

// TestAgentStandaloneCovPublicationRefusesASwappedTemporaryInode proves each
// publication refuses when the temporary it proved is replaced by another inode
// carrying the same bytes before the rename. Only the inode the writer itself
// filled and fsynced may become the permanent record; accepting a same-content
// stand-in would let anything that can create a file in the registry decide
// what the permanent name refers to.
func TestAgentStandaloneCovPublicationRefusesASwappedTemporaryInode(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	for _, publication := range agentStandaloneCovPublications(t, directory) {
		t.Run(publication.name, func(t *testing.T) {
			agentStandaloneCovRestorePublicationSeams(t)
			agentStandaloneCloseTemporary = func(file *os.File) error {
				require.NoError(t, file.Close())
				path := filepath.Join(directory.Name(), file.Name())
				payload, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, payload, 0o600))

				return os.Chmod(path, 0o600)
			}

			require.ErrorContains(t, publication.publish(directory), publication.swappedWant)
			require.Empty(t, agentStandaloneCovTemporaries(t, directory))
			require.NoError(t, os.Remove(filepath.Join(directory.Name(), publication.published)))
		})
	}
}

// TestAgentStandaloneCovPublicationRefusesARewrittenPayload proves each
// publication reads back what it published and refuses when the bytes are not
// the bytes it wrote. The readback is the only thing standing between a
// tampered temporary and a permanent authority record, so it must reject an
// append it never made — as a parse refusal where the record has a schema, and
// as a payload comparison where it does not.
func TestAgentStandaloneCovPublicationRefusesARewrittenPayload(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	for _, publication := range agentStandaloneCovPublications(t, directory) {
		t.Run(publication.name, func(t *testing.T) {
			agentStandaloneCovRestorePublicationSeams(t)
			agentStandaloneCloseTemporary = func(file *os.File) error {
				require.NoError(t, file.Close())
				path := filepath.Join(directory.Name(), file.Name())
				tampered, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
				require.NoError(t, openErr)
				_, writeErr := tampered.WriteString("tampered\n")
				require.NoError(t, writeErr)

				return tampered.Close()
			}

			require.ErrorContains(t, publication.publish(directory), publication.appendWant)
			require.Empty(t, agentStandaloneCovTemporaries(t, directory))
			require.NoError(t, os.Remove(filepath.Join(directory.Name(), publication.published)))
		})
	}
}

// TestAgentStandaloneCovSessionKeyRefusesAnUnencodableOwner proves the session
// key is only ever derived from an owner tuple that encodes, and that a failure
// yields no key at all. A caller that received a partial key would publish an
// ACTIVE marker no later claim could match against its own owner binding.
func TestAgentStandaloneCovSessionKeyRefusesAnUnencodableOwner(t *testing.T) {
	agentStandaloneCovRestorePublicationSeams(t)
	wantErr := errors.New("injected owner encoding failure")
	agentStandaloneMarshal = func(any) ([]byte, error) { return nil, wantErr }

	key, err := agentStandaloneSessionKey(
		agentStandaloneCovOwner(62711, 62712, "cov-session-key", "/srv/claude/cov-session-key", 1, 2),
	)
	require.ErrorIs(t, err, wantErr)
	require.Empty(t, key)
}

// TestAgentStandaloneCovRegistryFileReadFailsClosed proves the trusted registry
// reader refuses when the inode it opened cannot be described, when its payload
// cannot be read to the end, and when the inode's metadata no longer matches
// what was validated once the payload has been read. Returning a payload in any
// of those cases would hand the caller bytes it cannot attribute to the file it
// validated.
func TestAgentStandaloneCovRegistryFileReadFailsClosed(t *testing.T) {
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()

	t.Run("inode cannot be described", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		agentStandaloneCovWriteRegistryFile(t, directory, "62721.owner", "{}\n")
		agentStandaloneCovRestorePublicationSeams(t)
		wantErr := errors.New("injected registry stat failure")
		agentStandaloneFstat = func(int, *unix.Stat_t) error { return wantErr }

		payload, err := readAgentStandaloneFile(
			directory, "62721.owner", ownerUID, ownerGID, agentStandaloneOwnerMax,
		)
		require.ErrorIs(t, err, wantErr)
		require.Nil(t, payload)
	})

	t.Run("payload cannot be read", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		agentStandaloneCovWriteRegistryFile(t, directory, "62723.owner", "{}\n")
		agentStandaloneCovRestorePublicationSeams(t)
		wantErr := errors.New("injected registry read failure")
		agentStandaloneReadAll = func(io.Reader) ([]byte, error) { return nil, wantErr }

		payload, err := readAgentStandaloneFile(
			directory, "62723.owner", ownerUID, ownerGID, agentStandaloneOwnerMax,
		)
		require.ErrorIs(t, err, wantErr)
		require.Nil(t, payload)
	})

	t.Run("metadata changes under the read", func(t *testing.T) {
		directory := openAgentStandaloneTestDirectory(t)
		path := agentStandaloneCovWriteRegistryFile(t, directory, "62725.owner", "{}\n")
		agentStandaloneCovRestorePublicationSeams(t)
		agentStandaloneReadAll = func(reader io.Reader) ([]byte, error) {
			require.NoError(t, os.Chmod(path, 0o640))

			return io.ReadAll(reader)
		}

		payload, err := readAgentStandaloneFile(
			directory, "62725.owner", ownerUID, ownerGID, agentStandaloneOwnerMax,
		)
		require.ErrorContains(t, err, "changed while its payload was read")
		require.Nil(t, payload)
	})
}

// TestAgentStandaloneCovTemporaryCleanupReportsAnUnlinkRefusal proves every
// temporary cleanup reports a refused unlink instead of returning success, and
// leaves the temporary on disk. Reporting a clean registry while a temporary
// survives would let the claim continue into a state its own audit refuses.
func TestAgentStandaloneCovTemporaryCleanupReportsAnUnlinkRefusal(t *testing.T) {
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	suffix := agentStandaloneCovSuffix
	for _, testCase := range []struct {
		name  string
		entry string
		clean func(directory *os.File, name string) error
	}{
		{
			name:  "owner temporary",
			entry: "62731.owner.next-" + suffix,
			clean: func(directory *os.File, name string) error {
				return cleanupAgentStandaloneOwnerTemporary(directory, name, ownerUID, ownerGID)
			},
		},
		{
			name:  "domain record temporary",
			entry: "domain.json.next-" + suffix,
			clean: func(directory *os.File, name string) error {
				return cleanupAgentStandaloneDomainTemporary(directory, name, ownerUID, ownerGID)
			},
		},
		{
			name:  "authority probe temporary",
			entry: ".authority-probe-" + suffix,
			clean: func(directory *os.File, name string) error {
				return cleanupAgentStandaloneProbeTemporary(directory, name, ownerUID, ownerGID)
			},
		},
		{
			name:  "marker temporary",
			entry: "62733.quarantine.next-" + suffix,
			clean: func(directory *os.File, name string) error {
				agentStandaloneCovPermanentLock(t, directory, "62733.lock")

				return cleanupAgentStandaloneMarkerTemporary(directory, 62733, name, ownerUID, ownerGID)
			},
		},
		{
			name:  "target marker temporary",
			entry: "62735.quarantine.next-" + suffix,
			clean: func(directory *os.File, name string) error {
				held := createAgentStandaloneTestLock(t, directory, "62735.lock", ownerUID, ownerGID)

				return cleanupAgentStandaloneTargetMarkerTemporaries(
					directory, 62735, held, ownerUID, ownerGID, time.Now().Add(time.Second), nil, nil,
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := openAgentStandaloneTestDirectory(t)
			path := agentStandaloneCovWriteRegistryFile(t, directory, testCase.entry, "partial")
			agentStandaloneCovRestorePublicationSeams(t)
			wantErr := errors.New("injected unlink refusal")
			agentStandaloneUnlinkat = func(int, string, int) error { return wantErr }

			require.ErrorIs(t, testCase.clean(directory, testCase.entry), wantErr)
			require.FileExists(t, path)
		})
	}
}

// TestAgentStandaloneCovTargetMarkerCleanupRequiresItsOwnUIDLockInode proves
// the target-marker cleanup refuses when the held UID lock descriptor cannot be
// described at all. The cleanup unlinks state belonging to the UID that
// descriptor is supposed to hold, so an unprovable descriptor must stop it.
func TestAgentStandaloneCovTargetMarkerCleanupRequiresItsOwnUIDLockInode(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	held := createAgentStandaloneTestLock(t, directory, "62741.lock", ownerUID, ownerGID)
	path := agentStandaloneCovWriteRegistryFile(
		t, directory, "62741.quarantine.next-"+agentStandaloneCovSuffix, "partial",
	)
	agentStandaloneCovRestorePublicationSeams(t)
	wantErr := errors.New("injected held lock stat failure")
	agentStandaloneFstat = func(int, *unix.Stat_t) error { return wantErr }

	require.ErrorIs(t, cleanupAgentStandaloneTargetMarkerTemporaries(
		directory, 62741, held, ownerUID, ownerGID, time.Now().Add(time.Second), nil, nil,
	), wantErr)
	require.FileExists(t, path)
}

// TestAgentStandaloneCovPermanentLockRequiresADescribableInode proves a
// permanent registry lock is only accepted when the descriptor it just opened
// can be described, so the named-inode proof is never skipped.
func TestAgentStandaloneCovPermanentLockRequiresADescribableInode(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	agentStandaloneCovPermanentLock(t, directory, "owners.lock")
	agentStandaloneCovRestorePublicationSeams(t)
	wantErr := errors.New("injected lock stat failure")
	agentStandaloneFstat = func(int, *unix.Stat_t) error { return wantErr }

	lock, err := openAgentStandaloneNamedLock(directory, "owners.lock", false, ownerUID, ownerGID)
	require.ErrorIs(t, err, wantErr)
	require.Nil(t, lock)
}

// TestAgentStandaloneCovStateRootBindAbortsWhenADescriptorStopsAnswering
// proves the state root walk refuses when an ancestor it already opened, or the
// state root itself, can no longer be described. The walk's whole purpose is to
// prove every component is protected root-owned storage, so an undescribable
// component must abort the bind rather than be walked through.
func TestAgentStandaloneCovStateRootBindAbortsWhenADescriptorStopsAnswering(t *testing.T) {
	uid, gid := agentStandaloneTestAuthorityIDs()
	stateRoot := createAgentStandaloneProtectedStateRoot(t, uid, gid)
	components := strings.Count(strings.TrimPrefix(stateRoot, "/"), "/") + 1

	for _, testCase := range []struct {
		name string
		call int
	}{
		{name: "ancestor", call: 1},
		{name: "state root", call: components + 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			agentStandaloneCovRestorePublicationSeams(t)
			wantErr := errors.New("injected " + testCase.name + " stat failure")
			previous := agentStandaloneFstat
			calls := 0
			agentStandaloneFstat = func(fd int, stat *unix.Stat_t) error {
				calls++
				if calls == testCase.call {
					return wantErr
				}

				return previous(fd, stat)
			}

			bound, err := bindAgentStandaloneStateRoot(stateRoot, uid, gid)
			require.ErrorIs(t, err, wantErr)
			require.Equal(t, agentStandaloneStateRoot{}, bound)
			require.Equal(t, testCase.call, calls)
		})
	}
}

// TestAgentStandaloneCovDomainRecordRefusesADifferentPublishedRecord proves the
// authority record publication compares the record it reads back against the
// bytes it wrote, and refuses a well-formed record that is not the one it
// published. Parsing alone is not enough: a peer's valid record must not be
// mistaken for this claim's own authority.
func TestAgentStandaloneCovDomainRecordRefusesADifferentPublishedRecord(t *testing.T) {
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	record, err := currentAgentAuthorityDomain(directory)
	require.NoError(t, err)
	record.AuthorityID = "0123456789abcdef0123456789abcdef"
	agentStandaloneCovRestorePublicationSeams(t)
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

	err = replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, record)
	require.ErrorContains(t, err, "published agent authority record payload changed")
	published, loadErr := loadAgentAuthorityDomainRecord(directory, ownerUID, ownerGID)
	require.NoError(t, loadErr)
	require.Equal(t, "fedcba9876543210fedcba9876543210", published.AuthorityID,
		"the refusal must be attributable to the record that actually landed")
}
