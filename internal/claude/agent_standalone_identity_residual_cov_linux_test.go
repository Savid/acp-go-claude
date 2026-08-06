//go:build linux

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentStandaloneResInitialPIDNamespace is the fixed inode the kernel gives the
// initial PID namespace, the only namespace in which every PID on the host is
// visible and an identity claim can therefore be proved vacant.
const agentStandaloneResInitialPIDNamespace = 0xeffffffc

// agentStandaloneResFaultReadlink makes the named symlink resolution fail and
// leaves every other resolution alone.
func agentStandaloneResFaultReadlink(t *testing.T, target string, verdict error) {
	t.Helper()
	previous := agentStandaloneReadlink
	t.Cleanup(func() { agentStandaloneReadlink = previous })
	agentStandaloneReadlink = func(path string) (string, error) {
		if path == target {
			return "", verdict
		}

		return previous(path)
	}
}

// agentStandaloneResFaultNamespaceStat makes the named namespace anchor
// unreadable and leaves every other stat alone.
func agentStandaloneResFaultNamespaceStat(t *testing.T, target string, verdict error) {
	t.Helper()
	previous := agentAuthorityDomainStat
	t.Cleanup(func() { agentAuthorityDomainStat = previous })
	agentAuthorityDomainStat = func(path string, stat *unix.Stat_t) error {
		if path == target {
			return verdict
		}

		return previous(path, stat)
	}
}

// agentStandaloneResNamespaceInode rewrites the inode this process appears to
// have in its PID namespace, for both the "self" anchors and the numeric anchor
// the binder resolves through /proc/self. The binder's verdict turns on that
// inode, and a test cannot enter or leave a PID namespace without
// CAP_SYS_ADMIN, which the coverage container does not grant, so the kernel's
// answer for that one field is the only thing worth substituting. Everything
// else the binder reads stays the real /proc of the running process.
func agentStandaloneResNamespaceInode(t *testing.T, ino uint64) {
	t.Helper()
	previous := agentAuthorityDomainStat
	t.Cleanup(func() { agentAuthorityDomainStat = previous })
	numeric := agentStandaloneResSelfNamespacePath()
	agentAuthorityDomainStat = func(path string, stat *unix.Stat_t) error {
		if err := previous(path, stat); err != nil {
			return err
		}
		if path == "/proc/self/ns/pid" || path == "/proc/self/ns/pid_for_children" || path == numeric {
			stat.Ino = ino
		}

		return nil
	}
}

// agentStandaloneResSelfNamespacePath is the PID namespace anchor the binder
// reads once it has proved which PID procfs says this process is.
func agentStandaloneResSelfNamespacePath() string {
	return filepath.Join("/proc", strconv.Itoa(os.Getpid()), "ns", "pid")
}

// TestAgentStandaloneResBinderRefusesEveryProcfsDisagreement proves the
// standalone authority binder refuses when its PID namespace cannot be
// established, when procfs will not name this process, when it names another
// process, when the namespace behind that name cannot be read, when that
// namespace is not the one this process reported for itself, and when PID 1 is
// not visible. It then proves the rule those checks exist to enforce, in both
// directions: the initial PID namespace may establish authority, and any other
// namespace may do so only from its own PID 1.
//
// The binder is what stops an agent inside a container's own PID namespace from
// minting standalone authority the host would honour, for an identity it cannot
// see and therefore cannot prove vacant. Both of its verdicts have to be
// asserted in one run: creating a PID namespace needs CAP_SYS_ADMIN, so under a
// nested namespace the refusal is exercised and the acceptance is not, and under
// the host PID namespace it is the other way round.
func TestAgentStandaloneResBinderRefusesEveryProcfsDisagreement(t *testing.T) {
	selfPID := strconv.Itoa(os.Getpid())

	t.Run("PID namespace cannot be established", func(t *testing.T) {
		wantErr := errors.New("injected PID namespace stat failure")
		agentStandaloneResFaultNamespaceStat(t, "/proc/self/ns/pid", wantErr)

		require.ErrorIs(t, validateAgentStandaloneBinder(), wantErr)
	})

	t.Run("procfs will not name this process", func(t *testing.T) {
		wantErr := errors.New("injected proc self readlink failure")
		agentStandaloneResFaultReadlink(t, "/proc/self", wantErr)

		require.ErrorIs(t, validateAgentStandaloneBinder(), wantErr)
	})

	t.Run("procfs names another process", func(t *testing.T) {
		previous := agentStandaloneReadlink
		t.Cleanup(func() { agentStandaloneReadlink = previous })
		agentStandaloneReadlink = func(path string) (string, error) {
			if path == "/proc/self" {
				return strconv.Itoa(os.Getpid() + 1), nil
			}

			return previous(path)
		}

		require.ErrorContains(t, validateAgentStandaloneBinder(),
			"standalone agent authority binder requires canonical procfs self identity",
		)
	})

	t.Run("named PID namespace cannot be read", func(t *testing.T) {
		wantErr := errors.New("injected numeric PID namespace stat failure")
		agentStandaloneResFaultNamespaceStat(t, agentStandaloneResSelfNamespacePath(), wantErr)

		require.ErrorIs(t, validateAgentStandaloneBinder(), wantErr)
	})

	t.Run("named PID namespace is not the one we reported", func(t *testing.T) {
		numeric := agentStandaloneResSelfNamespacePath()
		previous := agentAuthorityDomainStat
		t.Cleanup(func() { agentAuthorityDomainStat = previous })
		agentAuthorityDomainStat = func(path string, stat *unix.Stat_t) error {
			if err := previous(path, stat); err != nil {
				return err
			}
			if path == numeric {
				stat.Ino++
			}

			return nil
		}

		require.ErrorContains(t, validateAgentStandaloneBinder(),
			"standalone agent authority binder requires self and procfs PID namespaces to match",
		)
	})

	t.Run("PID 1 is not visible", func(t *testing.T) {
		wantErr := errors.New("injected init status read failure")
		previous := agentStandaloneReadFile
		t.Cleanup(func() { agentStandaloneReadFile = previous })
		agentStandaloneReadFile = func(path string) ([]byte, error) {
			if path == "/proc/1/status" {
				return nil, wantErr
			}

			return previous(path)
		}

		err := validateAgentStandaloneBinder()
		require.ErrorIs(t, err, wantErr)
		require.ErrorContains(t, err, "prove unrestricted root procfs visibility")
	})

	t.Run("initial PID namespace establishes authority", func(t *testing.T) {
		agentStandaloneResNamespaceInode(t, agentStandaloneResInitialPIDNamespace)

		require.NoError(t, validateAgentStandaloneBinder(),
			"the binder must admit the initial PID namespace, where every PID is visible",
		)
	})

	t.Run("non-initial PID namespace below its own PID 1", func(t *testing.T) {
		if os.Getpid() == 1 {
			t.Skip("this process is namespace PID 1, which may establish authority")
		}
		agentStandaloneResNamespaceInode(t, agentStandaloneResInitialPIDNamespace-1)

		require.ErrorContains(t, validateAgentStandaloneBinder(),
			"non-initial PID namespace may establish agent authority only from namespace PID 1",
		)
		require.Equal(t, selfPID, strconv.Itoa(os.Getpid()),
			"the refusal must be about the namespace, not about a changed PID",
		)
	})
}

// TestAgentStandaloneResDomainClaimHonoursTheBinderVerdict proves that both
// points at which the authority domain claim consults the binder are gated on
// its verdict, whichever PID namespace the suite itself runs in: the claim that
// is about to rebind a foreign authority record, and the claim that is about to
// mint the first one. When the binder refuses, neither claim writes a record;
// when it accepts, the same claim carries on to the durability probe. Proving
// both against one fixture is what shows the binder, and not some later
// refusal, is what decides the nested-namespace case.
func TestAgentStandaloneResDomainClaimHonoursTheBinderVerdict(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("this process is namespace PID 1, which may establish authority")
	}
	want := agentStandaloneCovStaticOwner(62971, 62972, "res-binder")
	probeErr := errors.New("injected post-binder probe failure")
	const refusal = "non-initial PID namespace may establish agent authority only from namespace PID 1"

	t.Run("rebind refused in a nested namespace", func(t *testing.T) {
		directory, ownerUID, ownerGID := agentStandaloneCovDivergentDomainFixture(t)
		record := filepath.Join(directory.Name(), agentStandaloneCovDomainRecord)
		before, err := os.ReadFile(record)
		require.NoError(t, err)
		agentStandaloneResNamespaceInode(t, agentStandaloneResInitialPIDNamespace-1)
		agentStandaloneCovFailingProbe(t, probeErr)

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, false, time.Now().Add(time.Second), nil, nil,
		)
		require.Nil(t, lease)
		require.ErrorContains(t, err, refusal)
		require.NotErrorIs(t, err, probeErr, "the binder must refuse before the probe is consulted")
		after, err := os.ReadFile(record)
		require.NoError(t, err)
		require.Equal(t, before, after, "a refused rebind must leave the foreign record intact")
		contender, err := openAgentStandaloneNamedLock(directory, "domain.lock", false, ownerUID, ownerGID)
		require.NoError(t, err)
		require.NoError(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB),
			"the refused claim must release the domain lease",
		)
		require.NoError(t, contender.Close())
	})

	t.Run("first claim refused in a nested namespace", func(t *testing.T) {
		directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)
		agentStandaloneResNamespaceInode(t, agentStandaloneResInitialPIDNamespace-1)
		agentStandaloneCovFailingProbe(t, probeErr)

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, false, time.Now().Add(time.Second), nil, nil,
		)
		require.Nil(t, lease)
		require.ErrorContains(t, err, refusal)
		require.NotErrorIs(t, err, probeErr, "the binder must refuse before the probe is consulted")
		require.NoFileExists(t, filepath.Join(directory.Name(), agentStandaloneCovDomainRecord),
			"a refused first claim must mint no authority record",
		)
	})

	t.Run("first claim admitted in the initial namespace", func(t *testing.T) {
		directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)
		agentStandaloneResNamespaceInode(t, agentStandaloneResInitialPIDNamespace)
		agentStandaloneCovFailingProbe(t, probeErr)

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, false, time.Now().Add(time.Second), nil, nil,
		)
		require.Nil(t, lease)
		require.ErrorIs(t, err, probeErr, "an admitted claim must reach the durability probe")
		require.NoFileExists(t, filepath.Join(directory.Name(), agentStandaloneCovDomainRecord),
			"a claim stopped by the probe must mint no authority record",
		)
	})
}

// TestAgentStandaloneResDomainClaimAbandonsALiveMarkerTemporaryOutOfBudget
// proves that a rebinding claim which finds a marker temporary still held by a
// live UID lock abandons the claim once its retry budget is gone, rather than
// removing a temporary another holder is still writing. The budget is exhausted
// inside the very refusal that reports the temporary busy — the flock that
// discovers the live holder — so the claim reaches its retry with nothing left
// to wait with, which is the one ordering the surrounding deadline checks
// cannot produce on their own.
func TestAgentStandaloneResDomainClaimAbandonsALiveMarkerTemporaryOutOfBudget(t *testing.T) {
	directory, ownerUID, ownerGID := agentStandaloneCovDivergentDomainFixture(t)
	agentStandaloneCovPermanentLock(t, directory, agentStandaloneCovOwnersLock)
	held := createAgentStandaloneTestLock(t, directory, "62975.lock", ownerUID, ownerGID)
	require.NoError(t, unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	temporary := agentStandaloneCovWriteRegistryFile(
		t, directory, "62975.quarantine.next-"+agentStandaloneCovSuffix, "partial",
	)
	deadline := time.Now().Add(200 * time.Millisecond)
	previous := agentStandaloneFlock
	t.Cleanup(func() { agentStandaloneFlock = previous })
	agentStandaloneFlock = func(fd, how int) error {
		err := previous(fd, how)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			time.Sleep(time.Until(deadline) + 50*time.Millisecond)
		}

		return err
	}

	lease, err := acquireAgentStandaloneDomain(
		directory, agentStandaloneCovStaticOwner(62975, 62976, "res-busy"),
		ownerUID, ownerGID, true, deadline, nil, nil,
	)
	require.Nil(t, lease)
	require.ErrorContains(t, err, "exceeded 30 seconds")
	require.FileExists(t, temporary, "a live marker temporary is never removed")
}
