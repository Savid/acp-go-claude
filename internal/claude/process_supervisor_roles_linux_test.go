//go:build linux

package claude

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const supervisorCovSessionKey = "claude-supervisor-cov-session"

// supervisorCovDescriptorCount reports how many descriptors this process holds.
// The supervisor legs under test each create up to nine descriptors before they
// fail, so conservation across a whole table is what proves none of them is
// abandoned on a failure path.
func supervisorCovDescriptorCount(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)

	return len(entries)
}

// supervisorCovClosedFile returns a descriptor the kernel no longer answers
// for, which is how these tests make a capability that looks present fail when
// it is actually used.
func supervisorCovClosedFile(t *testing.T) *os.File {
	t.Helper()

	file, err := os.Open(os.DevNull)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	return file
}

// supervisorCovBorrowedAuthority builds the on-disk authority a borrowed launch
// adopts: a trusted authority root holding a shared domain lease, an exclusive
// named identity lock for uid, and an ownerless ACTIVE disposition naming
// uid/gid. It returns the two open leases the parent would pass down.
func supervisorCovBorrowedAuthority(t *testing.T, uid, gid uint32) (*os.File, *os.File) {
	t.Helper()
	supervisorCovRequireRoot(t)
	restoreAgentIdentityLockTestSeams(t)

	root := configureAgentIdentityLockTestRoot(t)
	trustedUID := uint32(os.Geteuid())
	trustedGID := uint32(os.Getegid())

	directory, err := bootstrapAgentIdentityLockDirectory(root, trustedUID, trustedGID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	deadline := time.Now().Add(agentStandaloneClaimMax)
	domain, err := acquireAgentStandaloneDomain(
		directory, agentStandaloneOwner{}, trustedUID, trustedGID, true, deadline, nil, nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = domain.Close() })

	owners, err := openAgentStandaloneNamedLock(directory, "owners.lock", true, trustedUID, trustedGID)
	require.NoError(t, err)
	require.NoError(t, owners.Close())

	identity, err := openAgentStandaloneNamedLock(
		directory, strconv.FormatUint(uint64(uid), 10)+".lock", true, trustedUID, trustedGID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = identity.Close() })
	require.NoError(t, unix.Flock(int(identity.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	affinity, err := openAgentStandaloneNamedLock(
		directory, agentStandaloneAffinityLockName(supervisorCovSessionKey), true, trustedUID, trustedGID,
	)
	require.NoError(t, err)
	require.NoError(t, affinity.Close())

	require.NoError(t, publishAgentStandaloneActive(
		directory, uid, gid, trustedUID, trustedGID, supervisorCovSessionKey, deadline, nil, nil,
	))

	return identity, domain
}

// supervisorCovInheritDuplicates points the inherited-descriptor seam at fresh
// duplicates of the supplied leases, which is what the guardian would actually
// receive from the parent.
func supervisorCovInheritDuplicates(t *testing.T, identity, domain *os.File) {
	t.Helper()

	turnSupervisorOpenFile = func(fd uintptr, _ string) *os.File {
		source := identity
		if fd != 7 {
			source = domain
		}
		if source == nil {
			return nil
		}

		duplicate, err := duplicateAgentIdentityLock(source)
		require.NoError(t, err)

		return duplicate
	}
}

// TestAdoptTurnSupervisorBorrowedAuthorityRequiresBothLeasesAndTheDisposition
// proves a borrowed guardian adopts nothing it cannot prove. It holds no
// authority of its own, so an absent identity lease, an absent authority
// domain, or an on-disk disposition that does not match the config all have to
// abandon adoption — and the identity lease already taken must be released
// rather than held by a guardian that is about to exit.
func TestAdoptTurnSupervisorBorrowedAuthorityRequiresBothLeasesAndTheDisposition(t *testing.T) {
	supervisorCovRequireRoot(t)

	config := supervisorCovConfig()
	config.AuthorityOrigin = turnSupervisorBorrowed
	config.IdentityLock = true
	config.AuthorityDomain = true
	config.Isolation.StandaloneOwnerID = ""
	config.Isolation.StandaloneStateRoot = ""

	t.Run("no identity lease", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)
		turnSupervisorOpenFile = func(uintptr, string) *os.File { return nil }

		identity, domain, err := adoptTurnSupervisorBorrowedAuthority(config, 7, 8)
		require.ErrorContains(t, err, "adopt Claude agent identity lock")
		require.Nil(t, identity)
		require.Nil(t, domain)
	})

	t.Run("no authority domain", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)
		lease, _ := supervisorCovBorrowedAuthority(t, config.Isolation.UID, config.Isolation.GID)
		supervisorCovInheritDuplicates(t, lease, nil)

		before := supervisorCovDescriptorCount(t)
		identity, domain, err := adoptTurnSupervisorBorrowedAuthority(config, 7, 8)
		require.ErrorContains(t, err, "adopt Claude agent authority domain")
		require.Nil(t, identity)
		require.Nil(t, domain)

		// The identity lease the guardian already adopted has to go back, or a
		// guardian that is about to exit would carry it out of reach of the
		// next turn.
		require.Equal(t, before, supervisorCovDescriptorCount(t))
	})

	t.Run("disposition does not match the config", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)
		identityLease, domainLease := supervisorCovBorrowedAuthority(
			t, config.Isolation.UID, config.Isolation.GID,
		)
		supervisorCovInheritDuplicates(t, identityLease, domainLease)

		mismatched := config
		mismatched.Isolation.GID = config.Isolation.GID + 1

		identity, domain, err := adoptTurnSupervisorBorrowedAuthority(mismatched, 7, 8)
		require.ErrorContains(t, err, "ownerless ACTIVE disposition")
		require.Nil(t, identity)
		require.Nil(t, domain)
	})

	t.Run("matching disposition", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)
		identityLease, domainLease := supervisorCovBorrowedAuthority(
			t, config.Isolation.UID, config.Isolation.GID,
		)
		supervisorCovInheritDuplicates(t, identityLease, domainLease)

		identity, domain, err := adoptTurnSupervisorBorrowedAuthority(config, 7, 8)
		require.NoError(t, err)
		require.NotNil(t, identity)
		require.NotNil(t, domain)
		t.Cleanup(func() {
			_ = identity.Close()
			_ = domain.Close()
		})

		// Adoption takes its own descriptors: the parent's leases stay exactly
		// as they were so the parent can release them independently.
		var adopted, held unix.Stat_t
		require.NoError(t, unix.Fstat(int(identity.file.Fd()), &adopted))
		require.NoError(t, unix.Fstat(int(identityLease.Fd()), &held))
		require.Equal(t, held.Ino, adopted.Ino)
		require.NotEqual(t, identityLease.Fd(), identity.file.Fd())
	})
}

// TestAcquireTurnSupervisorAuthorityRoutesByOrigin proves the guardian takes
// exactly one authority and takes it from the origin its config declares. A
// borrowed config adopts the parent's leases; a standalone config claims its
// own and reports the claim through the standalone handle, which is the handle
// that has to release both leases together.
func TestAcquireTurnSupervisorAuthorityRoutesByOrigin(t *testing.T) {
	supervisorCovRequireRoot(t)

	t.Run("borrowed adoption failure abandons the claim", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)
		turnSupervisorOpenFile = func(uintptr, string) *os.File { return nil }

		config := supervisorCovConfig()
		config.AuthorityOrigin = turnSupervisorBorrowed
		config.IdentityLock = true
		config.AuthorityDomain = true

		standalone := 0
		turnSupervisorAcquireStandalone = func(
			uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			standalone++

			return nil, errors.New("a borrowed config must never claim a standalone identity")
		}

		authority, err := acquireTurnSupervisorAuthority(config, 7, 8, nil, nil)
		require.ErrorContains(t, err, "adopt Claude agent identity lock")
		require.Nil(t, authority)
		require.Zero(t, standalone, "a borrowed config fell back to claiming a standalone identity")
	})

	t.Run("borrowed adoption", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)

		config := supervisorCovConfig()
		config.AuthorityOrigin = turnSupervisorBorrowed
		config.IdentityLock = true
		config.AuthorityDomain = true
		config.Isolation.StandaloneOwnerID = ""
		config.Isolation.StandaloneStateRoot = ""

		identityLease, domainLease := supervisorCovBorrowedAuthority(
			t, config.Isolation.UID, config.Isolation.GID,
		)
		supervisorCovInheritDuplicates(t, identityLease, domainLease)

		authority, err := acquireTurnSupervisorAuthority(config, 7, 8, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, authority)
		t.Cleanup(func() { _ = authority.Close() })
		require.NotNil(t, authority.identity)
		require.NotNil(t, authority.domain)
		require.Nil(t, authority.standalone, "a borrowed authority claimed a standalone handle")
	})

	t.Run("standalone claim failure", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)

		want := errors.New("claim")
		turnSupervisorAcquireStandalone = func(
			uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			return nil, want
		}

		authority, err := acquireTurnSupervisorAuthority(supervisorCovConfig(), 7, 8, nil, nil)
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "acquire Claude standalone agent identity authority")
		require.Nil(t, authority)
	})

	t.Run("standalone claim", func(t *testing.T) {
		restoreTurnSupervisorSeams(t)

		identity, err := os.CreateTemp(t.TempDir(), "identity")
		require.NoError(t, err)
		domain, err := os.CreateTemp(t.TempDir(), "domain")
		require.NoError(t, err)

		claimed := &agentStandaloneIdentity{
			identity: &agentIdentityLock{file: identity}, authority: &agentIdentityLock{file: domain},
		}
		config := supervisorCovConfig()

		var sawUID, sawGID uint32
		var sawOwner, sawRoot string
		turnSupervisorAcquireStandalone = func(
			uid uint32, gid uint32, owner string, stateRoot string,
			_ bool, _ string, _ <-chan struct{}, _ <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			sawUID, sawGID, sawOwner, sawRoot = uid, gid, owner, stateRoot

			return claimed, nil
		}

		authority, err := acquireTurnSupervisorAuthority(config, 7, 8, nil, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = authority.Close() })
		require.Equal(t, config.Isolation.UID, sawUID)
		require.Equal(t, config.Isolation.GID, sawGID)
		require.Equal(t, config.Isolation.StandaloneOwnerID, sawOwner)
		require.Equal(t, config.Isolation.StandaloneStateRoot, sawRoot)
		require.Same(t, claimed, authority.standalone)
		require.Same(t, claimed.identity, authority.identity)
		require.Same(t, claimed.authority, authority.domain)
	})
}

// TestStartTurnSupervisorLivenessReleasesEveryDescriptorOnAnyFailure proves the
// guardian abandons a liveness launch cleanly at every step. The launch builds
// a sealed config, three pipes and two authority duplicates before it forks, so
// a leg that returned without releasing them would leave the guardian holding
// pipe ends it will then wait on forever.
func TestStartTurnSupervisorLivenessReleasesEveryDescriptorOnAnyFailure(t *testing.T) {
	supervisorCovRequireRoot(t)
	restoreTurnSupervisorSeams(t)

	control, _ := supervisorCovPipe(t)
	completion, _ := supervisorCovPipe(t)

	lease, err := os.CreateTemp(t.TempDir(), "lease")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	live := &agentIdentityLock{file: lease}
	dead := &agentIdentityLock{file: supervisorCovClosedFile(t)}

	reset := func() {
		turnSupervisorMemfd = unix.MemfdCreate
		turnSupervisorWriteConfig = writeTurnSupervisorConfig
		turnSupervisorSealConfig = unix.FcntlInt
		turnSupervisorPipe = os.Pipe
		turnSupervisorExecutable = func() (string, error) { return "/bin/true", nil }
		turnSupervisorCommand = exec.Command
	}

	failingPipe := func(at int) func() (*os.File, *os.File, error) {
		calls := 0

		return func() (*os.File, *os.File, error) {
			calls++
			if calls == at {
				return nil, nil, errors.New("pipe")
			}

			return os.Pipe()
		}
	}

	before := supervisorCovDescriptorCount(t)

	for _, testCase := range []struct {
		name     string
		arrange  func()
		identity *agentIdentityLock
		domain   *agentIdentityLock
		want     string
	}{
		{
			name:    "config memfd",
			arrange: func() { turnSupervisorMemfd = func(string, int) (int, error) { return 0, errors.New("memfd") } },
			want:    "memfd",
		},
		{
			name: "config write",
			arrange: func() {
				turnSupervisorWriteConfig = func(io.WriteSeeker, turnSupervisorConfig) error {
					return errors.New("write config")
				}
			},
			want: "write config",
		},
		{
			name: "config seal",
			arrange: func() {
				turnSupervisorSealConfig = func(uintptr, int, int) (int, error) { return 0, errors.New("seal") }
			},
			want: "seal",
		},
		{name: "data pipe", arrange: func() { turnSupervisorPipe = failingPipe(1) }, want: "pipe"},
		{name: "peer pipe", arrange: func() { turnSupervisorPipe = failingPipe(2) }, want: "pipe"},
		{name: "start gate pipe", arrange: func() { turnSupervisorPipe = failingPipe(3) }, want: "pipe"},
		{
			name:     "identity duplicate",
			identity: dead,
			domain:   live,
			want:     "bad file descriptor",
		},
		{
			name:     "authority domain duplicate",
			identity: live,
			domain:   dead,
			want:     "bad file descriptor",
		},
		{
			name:    "supervisor executable",
			arrange: func() { turnSupervisorExecutable = func() (string, error) { return "", errors.New("executable") } },
			want:    "executable",
		},
		{
			name: "liveness launch",
			arrange: func() {
				absent := filepath.Join(t.TempDir(), "absent")
				turnSupervisorExecutable = func() (string, error) { return absent, nil }
			},
			want: "no such file or directory",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reset()

			if testCase.arrange != nil {
				testCase.arrange()
			}

			liveness, data, peer, start, startErr := startTurnSupervisorLiveness(
				supervisorCovConfig(), control, completion, testCase.identity, testCase.domain,
			)
			require.ErrorContains(t, startErr, testCase.want)
			require.Nil(t, liveness)
			require.Nil(t, data)
			require.Nil(t, peer)
			require.Nil(t, start)
		})
	}

	reset()
	require.Equal(
		t, before, supervisorCovDescriptorCount(t),
		"an abandoned liveness launch left descriptors behind",
	)
}

// TestStartTurnSupervisorLivenessCarriesTheAuthorityItWasGiven proves the
// liveness helper is told which authority it holds and never told to claim one
// itself. A borrowed guardian duplicates its two leases down and strips the
// standalone owner fields, so a liveness helper that still saw them would try
// to claim a second, competing identity for the same turn.
func TestStartTurnSupervisorLivenessCarriesTheAuthorityItWasGiven(t *testing.T) {
	supervisorCovRequireRoot(t)
	restoreTurnSupervisorSeams(t)

	control, _ := supervisorCovPipe(t)
	completion, _ := supervisorCovPipe(t)

	lease, err := os.CreateTemp(t.TempDir(), "lease")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	var sealed turnSupervisorConfig
	turnSupervisorWriteConfig = func(file io.WriteSeeker, config turnSupervisorConfig) error {
		sealed = config

		return writeTurnSupervisorConfig(file, config)
	}
	turnSupervisorExecutable = func() (string, error) { return "/bin/true", nil }

	var launched *exec.Cmd
	turnSupervisorCommand = func(name string, args ...string) *exec.Cmd {
		launched = exec.Command(name, args...)

		return launched
	}

	config := supervisorCovConfig()
	config.AuthorityOrigin = turnSupervisorBorrowed

	liveness, data, peer, start, err := startTurnSupervisorLiveness(
		config, control, completion,
		&agentIdentityLock{file: lease}, &agentIdentityLock{file: lease},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = data.Close()
		_ = peer.Close()
		_ = start.Close()
	})
	require.Same(t, launched, liveness)
	require.NoError(t, liveness.Wait())

	require.True(t, sealed.IdentityLock)
	require.True(t, sealed.AuthorityDomain)
	require.Empty(t, sealed.Isolation.StandaloneOwnerID)
	require.Empty(t, sealed.Isolation.StandaloneStateRoot)
	require.Nil(t, sealed.Isolation.IdentityLock)
	require.Nil(t, sealed.Isolation.AuthorityDomain)
	require.Len(t, launched.ExtraFiles, 8)
	require.Equal(
		t, []string{turnSupervisorModeEnv + "=" + turnSupervisorLivenessMode}, launched.Env,
	)
}

// TestRunTurnSupervisorLivenessRequiresItsWholeDescriptorSet proves the
// liveness helper refuses to run on a partial inheritance and releases whatever
// it did receive. The three descriptors are its completion proof, its guardian
// peer and its start gate; running without any one of them would mean launching
// the native root with no way to prove containment, no way to notice its
// guardian died, or no gate holding it until the parent is ready.
func TestRunTurnSupervisorLivenessRequiresItsWholeDescriptorSet(t *testing.T) {
	for _, missing := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(missing), func(t *testing.T) {
			restoreTurnSupervisorSeams(t)

			inherited := make([]*os.File, 0, 3)
			for range 3 {
				read, _ := supervisorCovPipe(t)
				inherited = append(inherited, read)
			}

			next := 0
			turnSupervisorOpenFile = func(uintptr, string) *os.File {
				file := inherited[next]
				next++
				if next-1 == missing {
					return nil
				}

				return file
			}

			err := runTurnSupervisorLiveness(strings.NewReader("{}"), strings.NewReader(""), io.Discard)
			require.ErrorContains(t, err, "Claude liveness inherited descriptors are unavailable")

			for index, file := range inherited {
				if index == missing {
					continue
				}
				require.Equal(
					t, ^uintptr(0), file.Fd(),
					"inherited descriptor %d survived the incomplete set", index,
				)
			}
		})
	}
}

// TestRunTurnSupervisorLivenessSealsEveryInheritedDescriptor proves each of the
// three inherited descriptors is sealed against exec before the native root is
// launched, and that a descriptor which cannot be sealed abandons the run. The
// native root is the agent's own process; inheriting the completion proof, the
// guardian peer or the start gate would hand it the supervisor's control plane.
func TestRunTurnSupervisorLivenessSealsEveryInheritedDescriptor(t *testing.T) {
	// Each descriptor is sealed with one F_GETFD and one F_SETFD, so faulting
	// the odd calls faults each descriptor in turn.
	for descriptor, faultAt := range map[string]int{"completion": 1, "guardian peer": 3, "start gate": 5} {
		t.Run(descriptor, func(t *testing.T) {
			restoreTurnSupervisorSeams(t)

			inherited := make([]*os.File, 0, 3)
			for range 3 {
				read, _ := supervisorCovPipe(t)
				inherited = append(inherited, read)
			}

			next := 0
			turnSupervisorOpenFile = func(uintptr, string) *os.File {
				file := inherited[next]
				next++

				return file
			}

			want := errors.New("seal")
			calls := 0
			turnSupervisorFcntl = func(uintptr, int, int) (int, error) {
				calls++
				if calls == faultAt {
					return 0, want
				}

				return 0, nil
			}

			require.ErrorIs(
				t,
				runTurnSupervisorLiveness(strings.NewReader("{}"), strings.NewReader(""), io.Discard),
				want,
			)
			require.Equal(t, faultAt, calls, "sealing continued past a refused descriptor")
		})
	}
}

// TestRunTurnSupervisorLivenessDelegatesWithItsOwnDescriptorLayout proves the
// liveness role hands the shared native body its own descriptor numbering. The
// guardian and the liveness helper inherit different layouts of the same
// authority, so delegating with the wrong numbers would make the helper adopt
// the wrong descriptors as identity and authority domain.
func TestRunTurnSupervisorLivenessDelegatesWithItsOwnDescriptorLayout(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	inherited := make([]*os.File, 0, 3)
	for range 3 {
		read, _ := supervisorCovPipe(t)
		inherited = append(inherited, read)
	}

	next := 0
	opened := make([]uintptr, 0, 3)
	turnSupervisorOpenFile = func(fd uintptr, _ string) *os.File {
		opened = append(opened, fd)
		file := inherited[next]
		next++

		return file
	}
	turnSupervisorFcntl = func(uintptr, int, int) (int, error) { return 0, nil }
	turnSupervisorSignalNotify = func(chan<- os.Signal, ...os.Signal) {}
	turnSupervisorSignalStop = func(chan<- os.Signal) {}

	err := runTurnSupervisorLiveness(strings.NewReader("not json"), strings.NewReader(""), io.Discard)
	require.ErrorContains(t, err, "decode Claude native supervisor config")
	require.Equal(t, []uintptr{8, 9, 10}, opened)
}
