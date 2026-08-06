//go:build linux

package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// identityLockCovSeams restores the agent identity lock seam group, including
// the descriptor seams this file adds, when the test ends.
func identityLockCovSeams(t *testing.T) {
	t.Helper()
	restoreAgentIdentityLockTestSeams(t)
	fstat := agentIdentityLockFstat
	openat := agentIdentityLockOpenat
	closeFD := agentIdentityLockCloseFD
	fcntl := agentIdentityLockFcntl
	closeLock := agentIdentityLockClose
	t.Cleanup(func() {
		agentIdentityLockFstat = fstat
		agentIdentityLockOpenat = openat
		agentIdentityLockCloseFD = closeFD
		agentIdentityLockFcntl = fcntl
		agentIdentityLockClose = closeLock
	})
}

// identityLockCovAuthority bootstraps a trusted authority root and returns its
// open directory descriptor together with the runtime root it lives under.
func identityLockCovAuthority(t *testing.T) (*os.File, string) {
	t.Helper()
	identityLockCovSeams(t)
	root := configureAgentIdentityLockTestRoot(t)
	directory, err := bootstrapAgentIdentityLockDirectory(
		root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	return directory, root
}

func identityLockCovNamedLock(t *testing.T, directory *os.File, name string) *os.File {
	t.Helper()
	file, err := openAgentStandaloneNamedLock(
		directory, name, true, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	return file
}

func identityLockCovLocked(t *testing.T, directory *os.File, name string, operation int) *os.File {
	t.Helper()
	file := identityLockCovNamedLock(t, directory, name)
	if err := unix.Flock(int(file.Fd()), operation|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	return file
}

func identityLockCovDuplicate(t *testing.T, source *os.File) *os.File {
	t.Helper()
	duplicate, err := duplicateAgentIdentityLock(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duplicate.Close() })

	return duplicate
}

func identityLockCovClosed(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "identity-lock-cov")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}

	return file
}

// identityLockCovFaultFstat makes the identity lock's descriptor metadata
// probe fail on its at-th call, standing in for a kernel that stops answering
// for a descriptor the adoption has already accepted. The caller restores the
// seam through identityLockCovSeams.
func identityLockCovFaultFstat(at int, failure error) {
	original := agentIdentityLockFstat
	calls := 0
	agentIdentityLockFstat = func(fd int, stat *unix.Stat_t) error {
		calls++
		if calls == at {
			return failure
		}

		return original(fd, stat)
	}
}

// identityLockCovFaultFstatat makes the named-entry probe fail on its at-th
// call. The first two calls always belong to the authority directory chain.
func identityLockCovFaultFstatat(at int, failure error) {
	original := agentIdentityDirectoryFstatat
	calls := 0
	agentIdentityDirectoryFstatat = func(dirfd int, path string, stat *unix.Stat_t, flags int) error {
		calls++
		if calls == at {
			return failure
		}

		return original(dirfd, path, stat, flags)
	}
}

// identityLockCovFaultClose makes the authority directory handoff close fail
// on its at-th call while still releasing the descriptor.
func identityLockCovFaultClose(at int, failure error) {
	calls := 0
	agentIdentityDirectoryClose = func(file *os.File) error {
		calls++
		if err := file.Close(); err != nil {
			return err
		}
		if calls == at {
			return failure
		}

		return nil
	}
}

// identityLockCovFdinfo renders an fdinfo payload claiming an flock of the
// given mode over the file's real inode, whether or not one is actually held.
func identityLockCovFdinfo(t *testing.T, file *os.File, mode string) []byte {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}

	return []byte(fmt.Sprintf(
		"pos:\t0\nlock:\t1: FLOCK ADVISORY %s %d 00:26:%d 0 EOF\n", mode, os.Getpid(), stat.Ino,
	))
}

type identityLockCovBorrowed struct {
	root          string
	authorityPath string
}

// identityLockCovBorrowedFixture establishes a complete borrowed authority
// root: a host-held domain lease, a host-held exclusive uid lock and an
// ownerless ACTIVE disposition for uid, which is the exact shape the borrowed
// disposition check is meant to accept.
func identityLockCovBorrowedFixture(t *testing.T, uid, gid uint32) identityLockCovBorrowed {
	t.Helper()
	identityLockCovSeams(t)
	root := configureAgentIdentityLockTestRoot(t)
	ownerUID, ownerGID := uint32(os.Geteuid()), uint32(os.Getegid())
	directory, err := bootstrapAgentIdentityLockDirectory(root, ownerUID, ownerGID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	deadline := time.Now().Add(agentStandaloneClaimMax)
	domainFile, err := acquireAgentStandaloneDomain(
		directory, agentStandaloneOwner{}, ownerUID, ownerGID, true, deadline, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = domainFile.Close() })
	owners, err := openAgentStandaloneNamedLock(directory, "owners.lock", true, ownerUID, ownerGID)
	if err != nil {
		t.Fatal(err)
	}
	if err = owners.Close(); err != nil {
		t.Fatal(err)
	}
	identityFile, err := openAgentStandaloneNamedLock(
		directory, strconv.FormatUint(uint64(uid), 10)+".lock", true, ownerUID, ownerGID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(identityFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = identityFile.Close() })
	const sessionKey = "borrowed-cov-session"
	affinity, err := openAgentStandaloneNamedLock(
		directory, agentStandaloneAffinityLockName(sessionKey), true, ownerUID, ownerGID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = affinity.Close(); err != nil {
		t.Fatal(err)
	}
	if err = publishAgentStandaloneActive(
		directory, uid, gid, ownerUID, ownerGID, sessionKey, deadline, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	authorityPath := filepath.Join(root, "acp-go", "agent-identities")

	return identityLockCovBorrowed{root: root, authorityPath: authorityPath}
}

// TestAgentIdentityAuthorityBootstrapHandoffFaultsFailClosed proves the
// two-step creation of the agent identity authority root never hands back a
// directory it could not fully establish: a failed creation, reopen, metadata
// probe or handoff close all abort with no directory returned, and a failed
// creation leaves the authority path absent rather than half made.
func TestAgentIdentityAuthorityBootstrapHandoffFaultsFailClosed(t *testing.T) {
	for name, testCase := range map[string]struct {
		fault  func(error)
		unmade string
	}{
		"owner directory handoff close": {
			fault: func(failure error) { identityLockCovFaultClose(2, failure) },
		},
		"authority directory handoff close": {
			fault: func(failure error) { identityLockCovFaultClose(4, failure) },
		},
		"authority directory creation": {
			fault: func(failure error) {
				original := agentIdentityDirectoryMkdirat
				calls := 0
				agentIdentityDirectoryMkdirat = func(dirfd int, path string, mode uint32) error {
					calls++
					if calls == 2 {
						return failure
					}

					return original(dirfd, path, mode)
				}
			},
			unmade: "agent-identities",
		},
		"owner directory open": {
			fault: func(failure error) {
				agentIdentityDirectoryOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
			unmade: "agent-identities",
		},
		"runtime root metadata": {
			fault:  func(failure error) { identityLockCovFaultFstat(1, failure) },
			unmade: ".",
		},
		"named owner directory metadata": {
			fault: func(failure error) { identityLockCovFaultFstat(3, failure) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			identityLockCovSeams(t)
			root := configureAgentIdentityLockTestRoot(t)
			failure := errors.New("injected " + name + " failure")
			testCase.fault(failure)
			directory, err := bootstrapAgentIdentityLockDirectory(
				root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
			)
			if directory != nil {
				_ = directory.Close()
				t.Fatal("authority bootstrap returned a directory despite an injected fault")
			}
			if !errors.Is(err, failure) {
				t.Fatalf("authority bootstrap error = %v, want %v", err, failure)
			}
			if testCase.unmade == "" {
				return
			}
			path := filepath.Join(root, "acp-go", testCase.unmade)
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed bootstrap left %s behind: %v", path, statErr)
			}
		})
	}

	t.Run("absent runtime root", func(t *testing.T) {
		identityLockCovSeams(t)
		root := filepath.Join(configureAgentIdentityLockTestRoot(t), "absent")
		_, err := bootstrapAgentIdentityLockDirectory(
			root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
		)
		if !errors.Is(err, unix.ENOENT) ||
			!strings.Contains(err.Error(), "open agent identity runtime root") {
			t.Fatalf("absent runtime root error = %v", err)
		}
	})
}

// TestAgentIdentityAuthorityOpenHandoffFaultsFailClosed proves that reopening
// an already-established authority root refuses whenever any step of the
// descriptor chain — the runtime root, either handoff close, or the authority
// directory itself — cannot be completed, so no caller ever operates on a
// partially proven authority.
func TestAgentIdentityAuthorityOpenHandoffFaultsFailClosed(t *testing.T) {
	const (
		uid = uint32(62451)
		gid = uint32(62452)
	)
	for name, testCase := range map[string]struct {
		prepare   func(root string, failure error) error
		absent    bool
		wantError string
	}{
		"absent runtime root": {
			absent: true,
		},
		"owner directory handoff close": {
			prepare: func(_ string, failure error) error {
				identityLockCovFaultClose(1, failure)

				return nil
			},
		},
		"authority directory handoff close": {
			prepare: func(_ string, failure error) error {
				identityLockCovFaultClose(2, failure)

				return nil
			},
		},
		"authority directory is gone": {
			prepare: func(root string, _ error) error {
				authority := filepath.Join(root, "acp-go", "agent-identities")

				return os.Rename(authority, authority+"-moved")
			},
			wantError: "open existing agent identity lock directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := identityLockCovBorrowedFixture(t, uid, gid)
			identityLockCovSeams(t)
			failure := errors.New("injected " + name + " failure")
			root := fixture.root
			if testCase.absent {
				root = filepath.Join(fixture.root, "absent")
			}
			if testCase.prepare != nil {
				if err := testCase.prepare(fixture.root, failure); err != nil {
					t.Fatal(err)
				}
			}
			err := validateBorrowedAgentIdentityDisposition(uid, gid, true, root)
			switch {
			case testCase.absent:
				if !errors.Is(err, unix.ENOENT) {
					t.Fatalf("authority open error = %v, want ENOENT", err)
				}
			case testCase.wantError != "":
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("authority open error = %v, want one containing %q", err, testCase.wantError)
				}
			default:
				if !errors.Is(err, failure) {
					t.Fatalf("authority open error = %v, want %v", err, failure)
				}
			}
		})
	}
}

// TestAgentIdentityLockDuplicationRefusesUnusableDescriptors proves the
// duplication path never fabricates a descriptor: a missing lock, a released
// lock and a closed descriptor are all refused, while a live lock duplicates
// onto the same inode as an independent descriptor. It also proves releasing
// an already-released lock is a no-op that reports no error.
func TestAgentIdentityLockDuplicationRefusesUnusableDescriptors(t *testing.T) {
	directory, _ := identityLockCovAuthority(t)
	if _, err := duplicateAgentIdentityLock(nil); err == nil ||
		!strings.Contains(err.Error(), "agent identity lock descriptor is required") {
		t.Fatalf("absent descriptor error = %v", err)
	}
	if _, err := duplicateAgentIdentityLock(identityLockCovClosed(t)); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closed descriptor error = %v, want EBADF", err)
	}
	for name, lock := range map[string]*agentIdentityLock{
		"released lock": {},
		"absent lock":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lock.Duplicate(); err == nil ||
				!strings.Contains(err.Error(), "agent identity lock is unavailable") {
				t.Fatalf("%s duplicate error = %v", name, err)
			}
			if err := lock.Close(); err != nil {
				t.Fatalf("%s close error = %v, want nil", name, err)
			}
		})
	}

	source := identityLockCovLocked(t, directory, "1260.lock", unix.LOCK_EX)
	lock := &agentIdentityLock{file: source}
	duplicate, err := lock.Duplicate()
	if err != nil {
		t.Fatalf("duplicate a live identity lock: %v", err)
	}
	defer duplicate.Close()
	var original, copied unix.Stat_t
	if err = unix.Fstat(int(source.Fd()), &original); err != nil {
		t.Fatal(err)
	}
	if err = unix.Fstat(int(duplicate.Fd()), &copied); err != nil {
		t.Fatal(err)
	}
	if original.Dev != copied.Dev || original.Ino != copied.Ino || duplicate.Fd() == source.Fd() {
		t.Fatalf("duplicate descriptor %d does not independently name the source inode", duplicate.Fd())
	}

	// Releasing a live lock reports the close failure and still forgets the
	// descriptor, so a failed release can never be retried onto a reused fd.
	releaseFailure := errors.New("injected identity lock release failure")
	agentIdentityLockClose = func(file *os.File) error {
		_ = file.Close()

		return releaseFailure
	}
	held := &agentIdentityLock{file: duplicate}
	if err = held.Close(); !errors.Is(err, releaseFailure) {
		t.Fatalf("release error = %v, want %v", err, releaseFailure)
	}
	if held.file != nil {
		t.Fatal("a failed release retained the identity lock descriptor")
	}
	if err = held.Close(); err != nil {
		t.Fatalf("second release error = %v, want nil", err)
	}
}

// TestAdoptAgentIdentityLockRefusesEveryUnprovenHandoff proves the adoption of
// an inherited uid lock re-proves every claim about the descriptor it is
// handed, and closes that descriptor on refusal instead of leaving a lease the
// caller believes was rejected.
func TestAdoptAgentIdentityLockRefusesEveryUnprovenHandoff(t *testing.T) {
	directory, root := identityLockCovAuthority(t)
	source := identityLockCovLocked(t, directory, "1261.lock", unix.LOCK_EX)
	identityLockCovNamedLock(t, directory, "1262.lock")

	if _, err := adoptAgentIdentityLock(nil, 1261, false, ""); err == nil ||
		!strings.Contains(err.Error(), "inherited agent identity lock descriptor is unavailable") {
		t.Fatalf("absent descriptor error = %v", err)
	}

	handed := identityLockCovDuplicate(t, source)
	if _, err := adoptAgentIdentityLock(handed, 1261, true, ""); err == nil ||
		!strings.Contains(err.Error(), "test agent identity lock root is required") {
		t.Fatalf("empty test root error = %v", err)
	}
	if err := handed.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("refused adoption left the handed descriptor open: %v", err)
	}

	handed = identityLockCovDuplicate(t, source)
	if _, err := adoptAgentIdentityLock(handed, 1261, false, root); err == nil ||
		!strings.Contains(err.Error(), "test agent identity lock root is forbidden") {
		t.Fatalf("forbidden test root error = %v", err)
	}
	if err := handed.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("refused adoption left the handed descriptor open: %v", err)
	}

	adopted, err := adoptAgentIdentityLock(identityLockCovDuplicate(t, source), 1261, true, root)
	if err != nil {
		t.Fatalf("adopt through the test authority root: %v", err)
	}
	if err = adopted.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err = adoptAgentIdentityLock(identityLockCovClosed(t), 1261, false, ""); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closed descriptor error = %v, want EBADF", err)
	}

	if _, err = adoptAgentIdentityLock(identityLockCovDuplicate(t, source), 1262, false, ""); err == nil ||
		!strings.Contains(err.Error(), "is not the trusted named lock 1262.lock") {
		t.Fatalf("mismatched named lock error = %v", err)
	}

	for name, fault := range map[string]func(error){
		"lock inode": func(failure error) { identityLockCovFaultFstat(7, failure) },
		"named lock resolution": func(failure error) {
			identityLockCovFaultFstatat(3, failure)
		},
		"flock state": func(failure error) {
			agentIdentityLockReadFile = func(string) ([]byte, error) { return nil, failure }
		},
		"close on exec flags": func(failure error) {
			agentIdentityLockFcntl = func(uintptr, int, int) (int, error) { return 0, failure }
		},
		"close on exec protection": func(failure error) {
			original := agentIdentityLockFcntl
			calls := 0
			agentIdentityLockFcntl = func(fd uintptr, request, argument int) (int, error) {
				calls++
				if calls == 2 {
					return 0, failure
				}

				return original(fd, request, argument)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			identityLockCovSeams(t)
			failure := errors.New("injected " + name + " failure")
			handed := identityLockCovDuplicate(t, source)
			fault(failure)
			if _, adoptErr := adoptAgentIdentityLock(handed, 1261, false, ""); !errors.Is(adoptErr, failure) {
				t.Fatalf("adoption error = %v, want %v", adoptErr, failure)
			}
			if closeErr := handed.Close(); !errors.Is(closeErr, os.ErrClosed) {
				t.Fatalf("refused adoption left the handed descriptor open: %v", closeErr)
			}
		})
	}

	var stat unix.Stat_t
	if err = unix.Fstat(int(source.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if err = validateInheritedAgentIdentityFlock(source, stat, "WRITE"); err != nil {
		t.Fatalf("refused adoptions mutated the host's exclusive lease: %v", err)
	}
}

// TestInheritedAgentIdentityLockOwnershipProofFailsClosed proves adoption does
// not believe the descriptor's own flock claim: it independently opens the
// trusted named lock and proves a fresh contender is blocked by it. Every way
// that proof can fail to complete — the contender cannot be opened, described,
// matched, contended, or is not blocked at all — refuses the handoff.
func TestInheritedAgentIdentityLockOwnershipProofFailsClosed(t *testing.T) {
	directory, root := identityLockCovAuthority(t)
	source := identityLockCovLocked(t, directory, "1270.lock", unix.LOCK_EX)
	identityLockCovNamedLock(t, directory, "1271.lock")
	unlocked := identityLockCovNamedLock(t, directory, "1272.lock")
	unlockedPath := filepath.Join(root, "acp-go", "agent-identities", "1272.lock")
	unlockedFdinfo := identityLockCovFdinfo(t, unlocked, "WRITE")

	for name, testCase := range map[string]struct {
		uid       uint32
		handed    string
		fault     func(error)
		wantError string
	}{
		"contender cannot be opened": {
			uid: 1270,
			fault: func(failure error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
		},
		"contender descriptor is unusable": {
			uid: 1270,
			fault: func(error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return 999999, nil
				}
			},
			wantError: "close inherited agent identity lock 1270.lock ownership contender",
		},
		"contender inode is not described": {
			uid:   1270,
			fault: func(failure error) { identityLockCovFaultFstat(9, failure) },
		},
		"contender is a different lock": {
			uid: 1270,
			fault: func(error) {
				original := agentIdentityLockOpenat
				agentIdentityLockOpenat = func(dirfd int, _ string, flags int, mode uint32) (int, error) {
					return original(dirfd, "1271.lock", flags, mode)
				}
			},
			wantError: "ownership contender is not the trusted named lock 1270.lock",
		},
		"contender is not blocked": {
			uid:    1272,
			handed: unlockedPath,
			fault: func(error) {
				agentIdentityLockReadFile = func(string) ([]byte, error) { return unlockedFdinfo, nil }
			},
			wantError: "inherited agent identity lock 1272.lock was not locked before handoff",
		},
		"contender cannot contend": {
			uid: 1270,
			fault: func(error) {
				agentIdentityLockOpenat = func(dirfd int, path string, _ int, _ uint32) (int, error) {
					return unix.Openat(dirfd, path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				}
			},
			wantError: "contend for inherited agent identity lock 1270.lock",
		},
	} {
		t.Run(name, func(t *testing.T) {
			identityLockCovSeams(t)
			failure := errors.New("injected " + name + " failure")
			handed := identityLockCovDuplicate(t, source)
			if testCase.handed != "" {
				opened, openErr := os.OpenFile(testCase.handed, os.O_RDWR, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				handed = opened
				t.Cleanup(func() { _ = handed.Close() })
			}
			testCase.fault(failure)
			_, adoptErr := adoptAgentIdentityLock(handed, testCase.uid, false, "")
			if testCase.wantError == "" {
				if !errors.Is(adoptErr, failure) {
					t.Fatalf("ownership proof error = %v, want %v", adoptErr, failure)
				}

				return
			}
			if adoptErr == nil || !strings.Contains(adoptErr.Error(), testCase.wantError) {
				t.Fatalf("ownership proof error = %v, want one containing %q", adoptErr, testCase.wantError)
			}
		})
	}

	var stat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if err := validateInheritedAgentIdentityFlock(source, stat, "WRITE"); err != nil {
		t.Fatalf("refused ownership proofs mutated the host's exclusive lease: %v", err)
	}
}

// identityLockCovDomainRecord publishes the running authority domain so a
// domain lock handoff can be adopted end to end.
func identityLockCovDomainRecord(t *testing.T, directory *os.File, root string) {
	t.Helper()
	record, err := currentAgentAuthorityDomain(directory)
	if err != nil {
		t.Fatal(err)
	}
	record.AuthorityID = authorityDomainCovID
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "acp-go", "agent-identities", agentAuthorityDomainRecordName)
	if err = os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptAgentAuthorityDomainRefusesEveryUnprovenHandoff proves the shared
// authority domain lease is adopted only when the descriptor is the trusted
// named domain.lock and is really holding a lease that blocks an exclusive
// contender, and that any step the kernel cannot complete refuses the handoff
// and closes the descriptor.
func TestAdoptAgentAuthorityDomainRefusesEveryUnprovenHandoff(t *testing.T) {
	directory, root := identityLockCovAuthority(t)
	identityLockCovDomainRecord(t, directory, root)
	source := identityLockCovLocked(t, directory, "domain.lock", unix.LOCK_SH)
	foreign := identityLockCovLocked(t, directory, "1280.lock", unix.LOCK_SH)

	if _, err := adoptAgentAuthorityDomain(nil, false, ""); err == nil ||
		!strings.Contains(err.Error(), "inherited agent authority domain descriptor is unavailable") {
		t.Fatalf("absent descriptor error = %v", err)
	}

	handed := identityLockCovDuplicate(t, source)
	if _, err := adoptAgentAuthorityDomain(handed, true, ""); err == nil ||
		!strings.Contains(err.Error(), "test agent identity lock root is required") {
		t.Fatalf("empty test root error = %v", err)
	}
	if err := handed.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("refused adoption left the handed descriptor open: %v", err)
	}

	handed = identityLockCovDuplicate(t, source)
	if _, err := adoptAgentAuthorityDomain(handed, false, root); err == nil ||
		!strings.Contains(err.Error(), "test agent identity lock root is forbidden") {
		t.Fatalf("forbidden test root error = %v", err)
	}

	adopted, err := adoptAgentAuthorityDomain(identityLockCovDuplicate(t, source), true, root)
	if err != nil {
		t.Fatalf("adopt through the test authority root: %v", err)
	}
	if err = adopted.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err = adoptAgentAuthorityDomain(identityLockCovClosed(t), false, ""); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closed descriptor error = %v, want EBADF", err)
	}

	if _, err = adoptAgentAuthorityDomain(identityLockCovDuplicate(t, foreign), false, ""); err == nil ||
		!strings.Contains(err.Error(), "is not the trusted named domain.lock") {
		t.Fatalf("foreign named lock error = %v", err)
	}

	for name, testCase := range map[string]struct {
		fault     func(error)
		wantError string
	}{
		"domain inode": {
			fault: func(failure error) { identityLockCovFaultFstat(7, failure) },
		},
		"named domain resolution": {
			fault: func(failure error) { identityLockCovFaultFstatat(3, failure) },
		},
		"contender cannot be opened": {
			fault: func(failure error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
		},
		"contender cannot contend": {
			fault: func(error) {
				agentIdentityLockOpenat = func(dirfd int, path string, _ int, _ uint32) (int, error) {
					return unix.Openat(dirfd, path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				}
			},
			wantError: "contend for inherited agent authority domain",
		},
		"contender cannot be released": {
			fault: func(failure error) {
				original := agentIdentityLockCloseFD
				agentIdentityLockCloseFD = func(fd int) error {
					_ = original(fd)

					return failure
				}
			},
		},
		"close on exec flags": {
			fault: func(failure error) {
				agentIdentityLockFcntl = func(uintptr, int, int) (int, error) { return 0, failure }
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			identityLockCovSeams(t)
			failure := errors.New("injected " + name + " failure")
			handed := identityLockCovDuplicate(t, source)
			testCase.fault(failure)
			_, adoptErr := adoptAgentAuthorityDomain(handed, false, "")
			switch {
			case testCase.wantError != "":
				if adoptErr == nil || !strings.Contains(adoptErr.Error(), testCase.wantError) {
					t.Fatalf("domain adoption error = %v, want one containing %q", adoptErr, testCase.wantError)
				}
			default:
				if !errors.Is(adoptErr, failure) {
					t.Fatalf("domain adoption error = %v, want %v", adoptErr, failure)
				}
			}
			if closeErr := handed.Close(); !errors.Is(closeErr, os.ErrClosed) {
				t.Fatalf("refused adoption left the handed descriptor open: %v", closeErr)
			}
		})
	}

	t.Run("domain was not locked", func(t *testing.T) {
		identityLockCovSeams(t)
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		handed, openErr := os.OpenFile(
			filepath.Join(root, "acp-go", "agent-identities", "domain.lock"), os.O_RDWR, 0,
		)
		if openErr != nil {
			t.Fatal(openErr)
		}
		t.Cleanup(func() { _ = handed.Close() })
		payload := identityLockCovFdinfo(t, handed, "READ")
		agentIdentityLockReadFile = func(string) ([]byte, error) { return payload, nil }
		if _, adoptErr := adoptAgentAuthorityDomain(handed, false, ""); adoptErr == nil ||
			!strings.Contains(adoptErr.Error(), "was not locked before handoff") {
			t.Fatalf("unlocked domain error = %v", adoptErr)
		}
	})
}

// TestBorrowedAgentIdentityDispositionRefusesUnprovenModes proves the borrowed
// disposition check only runs against the authority root it was told to use,
// and refuses when the authority root cannot be enumerated or the owner
// binding cannot be resolved, rather than reading either as "nothing found".
func TestBorrowedAgentIdentityDispositionRefusesUnprovenModes(t *testing.T) {
	const (
		uid = uint32(62461)
		gid = uint32(62462)
	)
	t.Run("test root is required", func(t *testing.T) {
		identityLockCovBorrowedFixture(t, uid, gid)
		if err := validateBorrowedAgentIdentityDisposition(uid, gid, true, ""); err == nil ||
			!strings.Contains(err.Error(), "test agent identity lock root is required") {
			t.Fatalf("empty test root error = %v", err)
		}
	})

	t.Run("test root is forbidden", func(t *testing.T) {
		fixture := identityLockCovBorrowedFixture(t, uid, gid)
		if err := validateBorrowedAgentIdentityDisposition(uid, gid, false, fixture.root); err == nil ||
			!strings.Contains(err.Error(), "test agent identity lock root is forbidden") {
			t.Fatalf("forbidden test root error = %v", err)
		}
	})

	t.Run("owner binding cannot be resolved", func(t *testing.T) {
		fixture := identityLockCovBorrowedFixture(t, uid, gid)
		identityLockCovSeams(t)
		failure := errors.New("injected owner resolution failure")
		identityLockCovFaultFstatat(3, failure)
		if err := validateBorrowedAgentIdentityDisposition(uid, gid, true, fixture.root); !errors.Is(err, failure) {
			t.Fatalf("owner resolution error = %v, want %v", err, failure)
		}
	})

	t.Run("owner binding is not parseable", func(t *testing.T) {
		fixture := identityLockCovBorrowedFixture(t, uid, gid)
		owner := filepath.Join(fixture.authorityPath, strconv.FormatUint(uint64(uid), 10)+".owner")
		if err := os.WriteFile(owner, []byte("bound\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := validateBorrowedAgentIdentityDisposition(uid, gid, true, fixture.root)
		if err == nil || !strings.Contains(err.Error(), "audit borrowed agent identity authority") ||
			!strings.Contains(err.Error(), "looking for beginning of value") {
			t.Fatalf("unparsable owner binding error = %v", err)
		}
	})

	t.Run("authority root cannot be enumerated", func(t *testing.T) {
		identityLockCovSeams(t)
		root := configureAgentIdentityLockTestRoot(t)
		directory, err := bootstrapAgentIdentityLockDirectory(
			root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err = directory.Close(); err != nil {
			t.Fatal(err)
		}
		if err = rejectBorrowedAgentIdentityTemporaries(directory); !errors.Is(err, unix.EBADF) {
			t.Fatalf("unenumerable authority error = %v, want EBADF", err)
		}
	})
}

// TestInheritedAgentIdentityFlockStateMustBeFullyParseable proves the flock
// entry a descriptor reports must be readable at all and must be a fully
// parseable advisory record: an unreadable fdinfo, a non-numeric sequence, a
// negative or non-numeric owner, and an inode identity that is not exactly
// major:minor:inode are all refused.
func TestInheritedAgentIdentityFlockStateMustBeFullyParseable(t *testing.T) {
	identityLockCovSeams(t)
	readFailure := errors.New("injected fdinfo read failure")
	agentIdentityLockReadFile = func(string) ([]byte, error) { return nil, readFailure }
	if err := validateInheritedAgentIdentityFlock(os.Stdin, unix.Stat_t{}, "WRITE"); !errors.Is(err, readFailure) ||
		!strings.Contains(err.Error(), "read inherited agent identity flock state") {
		t.Fatalf("unreadable fdinfo error = %v, want %v", err, readFailure)
	}

	descriptor := unix.Stat_t{Dev: unix.Mkdev(0, 0x26), Ino: 52599113}
	valid := strings.Fields("lock: 1: FLOCK ADVISORY WRITE 0 00:26:52599113 0 EOF")
	if err := validateInheritedAgentIdentityFlockLine(valid, descriptor, "WRITE"); err != nil {
		t.Fatalf("validate a well formed flock entry: %v", err)
	}
	for name, testCase := range map[string]struct {
		index     int
		value     string
		wantError string
	}{
		"sequence is not numeric":     {index: 1, value: "zz:", wantError: "malformed flock sequence"},
		"owner is not numeric":        {index: 5, value: "owner", wantError: "malformed flock owner"},
		"owner is negative":           {index: 5, value: "-1", wantError: "malformed flock owner"},
		"inode identity is truncated": {index: 6, value: "00:26", wantError: "malformed flock inode"},
	} {
		t.Run(name, func(t *testing.T) {
			fields := append([]string(nil), valid...)
			fields[testCase.index] = testCase.value
			err := validateInheritedAgentIdentityFlockLine(fields, descriptor, "WRITE")
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("flock entry error = %v, want one containing %q", err, testCase.wantError)
			}
		})
	}
}

// TestAdoptedStandaloneAgentIdentityDispositionRefusesEveryDrift proves a
// standalone identity is only re-adopted when the authority root it was told
// to use can be opened, carries no unresolved temporary, audits clean, and
// still holds the exact immutable owner binding the supervisor claims. Each
// failure is attributed to its own stage rather than absorbed into a generic
// refusal.
func TestAdoptedStandaloneAgentIdentityDispositionRefusesEveryDrift(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("standalone disposition drift requires a root-owned authority and a distinct native identity")
	}
	const (
		uid = uint32(62471)
		gid = uint32(62472)
	)
	acquire := func(t *testing.T) (string, string) {
		t.Helper()
		identityLockCovSeams(t)
		root := configureAgentIdentityLockTestRoot(t)
		stateRoot := createAgentStandaloneProtectedStateRoot(t, uid, gid)
		standalone, err := acquireAgentStandaloneIdentity(
			uid, gid, "standalone-drift", stateRoot, true, root, make(chan struct{}), make(chan os.Signal),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = standalone.Close() })

		return root, stateRoot
	}
	validate := func(root, stateRoot string, testOnly bool, passedRoot string) error {
		return validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "standalone-drift", stateRoot, testOnly, passedRoot,
		)
	}

	t.Run("sealed disposition is re-adopted", func(t *testing.T) {
		root, stateRoot := acquire(t)
		if err := validate(root, stateRoot, true, root); err != nil {
			t.Fatalf("re-adopt the sealed standalone disposition: %v", err)
		}
	})

	t.Run("test root is required", func(t *testing.T) {
		root, stateRoot := acquire(t)
		if err := validate(root, stateRoot, true, ""); err == nil ||
			!strings.Contains(err.Error(), "test agent identity lock root is required") {
			t.Fatalf("empty test root error = %v", err)
		}
	})

	t.Run("test root is forbidden", func(t *testing.T) {
		root, stateRoot := acquire(t)
		if err := validate(root, stateRoot, false, root); err == nil ||
			!strings.Contains(err.Error(), "test agent identity lock root is forbidden") {
			t.Fatalf("forbidden test root error = %v", err)
		}
	})

	t.Run("authority root is absent", func(t *testing.T) {
		root, stateRoot := acquire(t)
		if err := validate(root, stateRoot, true, filepath.Join(root, "absent")); !errors.Is(err, unix.ENOENT) {
			t.Fatalf("absent authority root error = %v, want ENOENT", err)
		}
	})

	t.Run("authority root holds an unresolved temporary", func(t *testing.T) {
		root, stateRoot := acquire(t)
		temporary := filepath.Join(
			root, "acp-go", "agent-identities", "domain.json.next-0123456789abcdef01234567",
		)
		if err := os.WriteFile(temporary, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validate(root, stateRoot, true, root); err == nil ||
			!strings.Contains(err.Error(), "unresolved temporary") {
			t.Fatalf("unresolved temporary error = %v", err)
		}
	})

	t.Run("authority root no longer audits clean", func(t *testing.T) {
		root, stateRoot := acquire(t)
		owners := filepath.Join(root, "acp-go", "agent-identities", "owners.lock")
		if err := os.Chmod(owners, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validate(root, stateRoot, true, root); err == nil ||
			!strings.Contains(err.Error(), "mode is 0644, want 0600") {
			t.Fatalf("group-readable owners lock error = %v", err)
		}
	})

	// An authority root can audit clean and still have lost this uid's owner
	// binding: removing both the binding and its retained marker leaves a
	// registry the audit accepts, so the binding check is the only thing
	// standing between the supervisor and an unbound identity.
	t.Run("owner binding is gone from a clean authority", func(t *testing.T) {
		root, stateRoot := acquire(t)
		authority := filepath.Join(root, "acp-go", "agent-identities")
		for _, name := range []string{
			strconv.FormatUint(uint64(uid), 10) + ".owner",
			strconv.FormatUint(uint64(uid), 10) + ".quarantine",
		} {
			if err := os.Remove(filepath.Join(authority, name)); err != nil {
				t.Fatal(err)
			}
		}
		if err := validate(root, stateRoot, true, root); !errors.Is(err, unix.ENOENT) {
			t.Fatalf("absent owner binding error = %v, want ENOENT", err)
		}
	})

	t.Run("owner binding names another owner", func(t *testing.T) {
		root, stateRoot := acquire(t)
		err := validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "another-owner", stateRoot, true, root,
		)
		if err == nil ||
			!strings.Contains(err.Error(), "does not match its immutable owner binding") {
			t.Fatalf("drifted owner id error = %v", err)
		}
	})

	t.Run("permanently bound identity cannot be borrowed", func(t *testing.T) {
		root, _ := acquire(t)
		err := validateBorrowedAgentIdentityDisposition(uid, gid, true, root)
		if err == nil || !strings.Contains(err.Error(), "has a permanent owner binding") {
			t.Fatalf("permanently bound identity error = %v", err)
		}
	})

	t.Run("adopted authority origin selects its own proof", func(t *testing.T) {
		root, stateRoot := acquire(t)
		standalone := turnSupervisorConfig{
			AuthorityOrigin: turnSupervisorStandalone,
			Isolation: ProcessIsolation{
				UID: uid, GID: gid,
				StandaloneOwnerID:   "standalone-drift",
				StandaloneStateRoot: stateRoot,
			},
		}
		if err := validateTurnSupervisorAdoptedAuthorityDisposition(standalone, true, root); err != nil {
			t.Fatalf("standalone origin proof: %v", err)
		}
		borrowed := standalone
		borrowed.AuthorityOrigin = turnSupervisorBorrowed
		if err := validateTurnSupervisorAdoptedAuthorityDisposition(borrowed, true, root); err == nil ||
			!strings.Contains(err.Error(), "has a permanent owner binding") {
			t.Fatalf("borrowed origin proof = %v", err)
		}
		unknown := standalone
		unknown.AuthorityOrigin = "inherited"
		if err := validateTurnSupervisorAdoptedAuthorityDisposition(unknown, true, root); err == nil ||
			!strings.Contains(err.Error(), "adopted authority origin is invalid") {
			t.Fatalf("unknown origin proof = %v", err)
		}
	})
}
