//go:build linux

package claude

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	nativeOwnershipCovUID = uint32(65534)
	nativeOwnershipCovGID = uint32(65534)
)

func nativeOwnershipCovRequireRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("generated native ownership handoff requires the trusted root identity")
	}
}

// nativeOwnershipCovRoot builds the tree shape the handoff accepts: a 0711
// caller root under the sticky system temp root, holding a 0700 generated root.
// A checkout cannot serve as the parent because Ubuntu creates home directories
// mode 0750, which no other identity can search into.
func nativeOwnershipCovRoot(t *testing.T) string {
	t.Helper()
	nativeOwnershipCovRequireRoot(t)

	parent, err := os.MkdirTemp("/tmp", "acp-go-claude-generated-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	require.NoError(t, os.Chmod(parent, 0o711))

	native := filepath.Join(parent, "native")
	require.NoError(t, os.Mkdir(native, 0o700))

	return native
}

func nativeOwnershipCovIsolation() *ProcessIsolation {
	return &ProcessIsolation{
		UID: nativeOwnershipCovUID, GID: nativeOwnershipCovGID,
		BaseEnvironment: map[string]string{},
	}
}

func nativeOwnershipCovOwner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()

	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(path, &stat))

	return stat.Uid, stat.Gid
}

func nativeOwnershipCovRequireTrusted(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		uid, gid := nativeOwnershipCovOwner(t, path)
		require.Equal(t, uint32(0), uid, "%s was handed to the dropped identity", path)
		require.Equal(t, uint32(0), gid, "%s was handed to the dropped identity", path)
	}
}

// nativeOwnershipCovPathDescriptor returns an O_PATH descriptor. O_PATH answers
// fstat but refuses every operation that reads or writes the inode, which is how
// these tests make an already-accepted descriptor stop answering without racing
// the filesystem.
func nativeOwnershipCovPathDescriptor(t *testing.T, path string, directory bool) *os.File {
	t.Helper()

	flags := unix.O_PATH | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}

	fd, err := unix.Open(path, flags, 0)
	require.NoError(t, err)

	file := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { _ = file.Close() })

	return file
}

// nativeOwnershipCovFaultFstat stages a second read of an inode. The handoff
// always re-reads a descriptor it already accepted, so amend lets a test make
// the later read disagree with the earlier one.
func nativeOwnershipCovFaultFstat(t *testing.T, amend func(calls int, stat *unix.Stat_t) error) {
	t.Helper()

	baseline := nativeOwnershipFstat
	calls := 0
	nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
		calls++
		if err := baseline(fd, stat); err != nil {
			return err
		}

		return amend(calls, stat)
	}

	t.Cleanup(func() { nativeOwnershipFstat = baseline })
}

// TestGeneratedNativeTreeHandoffBindsToAnAbsoluteRootOrRefuses proves the
// handoff only runs when it is both wanted and anchored. Without isolation there
// is no dropped identity to hand anything to, and a relative root would resolve
// against the working directory the agent controls rather than the tree the
// caller named.
func TestGeneratedNativeTreeHandoffBindsToAnAbsoluteRootOrRefuses(t *testing.T) {
	require.NoError(t, handoffGeneratedNativeTree("/nonexistent", nil))

	require.ErrorContains(
		t,
		handoffGeneratedNativeTree("relative/native", nativeOwnershipCovIsolation()),
		"generated native path must be absolute",
	)
}

// TestGeneratedNativeTreeHandoffPropagatesAMissingRoot proves an absent
// generated root surfaces the kernel's own refusal rather than being treated as
// an empty tree that needed no handoff.
func TestGeneratedNativeTreeHandoffPropagatesAMissingRoot(t *testing.T) {
	native := nativeOwnershipCovRoot(t)

	err := handoffGeneratedNativeTree(
		filepath.Join(filepath.Dir(native), "absent"), nativeOwnershipCovIsolation(),
	)
	require.ErrorIs(t, err, unix.ENOENT)
}

// TestGeneratedNativeTreeHandoffTransfersTheWholeTreeAndNothingAbove proves the
// handoff is recursive and bounded: every inode inside the generated root
// changes owner with its mode intact, and the caller root that contains it does
// not.
func TestGeneratedNativeTreeHandoffTransfersTheWholeTreeAndNothingAbove(t *testing.T) {
	native := nativeOwnershipCovRoot(t)
	parent := filepath.Dir(native)
	nested := filepath.Join(native, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	leaf := filepath.Join(nested, "leaf")
	require.NoError(t, os.WriteFile(leaf, []byte("x"), 0o600))
	executable := filepath.Join(native, "runner")
	require.NoError(t, os.WriteFile(executable, []byte("x"), 0o700))

	isolation := nativeOwnershipCovIsolation()
	require.NoError(t, handoffGeneratedNativeTree(native, isolation))

	for path, mode := range map[string]os.FileMode{
		native: 0o700, nested: 0o700, leaf: 0o600, executable: 0o700,
	} {
		uid, gid := nativeOwnershipCovOwner(t, path)
		require.Equal(t, isolation.UID, uid, path)
		require.Equal(t, isolation.GID, gid, path)

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm(), path)
	}

	nativeOwnershipCovRequireTrusted(t, parent)
}

// TestGeneratedNativeAncestorRefusalsAreStated pins the exact reason the
// generated-tree ancestry validator refuses each unsafe shape. These reasons are
// the containment contract: an ancestor that is not trusted-owned, a generated
// root that is not exactly 0700, a writable ancestor without sticky protection,
// or an ancestor the dropped identity could not have entered.
func TestGeneratedNativeAncestorRefusalsAreStated(t *testing.T) {
	const (
		trustedUID = uint32(0)
		trustedGID = uint32(0)
	)

	directory := func(mode, uid, gid uint32) unix.Stat_t {
		return unix.Stat_t{Mode: unix.S_IFDIR | mode, Uid: uid, Gid: gid}
	}

	for _, testCase := range []struct {
		name  string
		stat  unix.Stat_t
		final bool
		want  string
	}{
		{
			name: "not a directory",
			stat: unix.Stat_t{Mode: unix.S_IFREG | 0o700},
			want: "ancestry is not a trusted directory",
		},
		{
			name: "ancestor owned by the dropped identity",
			stat: directory(0o755, nativeOwnershipCovUID, nativeOwnershipCovGID),
			want: "ancestry is not a trusted directory",
		},
		{
			name:  "generated root is not exactly 0700",
			stat:  directory(0o750, trustedUID, trustedGID),
			final: true,
			want:  "generated native root mode 0750 is unsafe",
		},
		{
			name: "group-writable ancestor without sticky protection",
			stat: directory(0o771, trustedUID, trustedGID),
			want: "0771 is writable without sticky protection",
		},
		{
			name: "ancestor the dropped identity cannot traverse",
			stat: directory(0o700, trustedUID, trustedGID),
			want: "not traversable by the target identity",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateGeneratedNativeAncestor(
				testCase.stat, testCase.final, trustedUID, trustedGID,
				nativeOwnershipCovUID, nativeOwnershipCovGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o711, trustedUID, trustedGID), false, trustedUID, trustedGID,
		nativeOwnershipCovUID, nativeOwnershipCovGID,
	))
	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o1777, trustedUID, trustedGID), false, trustedUID, trustedGID,
		nativeOwnershipCovUID, nativeOwnershipCovGID,
	))
	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o700, trustedUID, trustedGID), true, trustedUID, trustedGID,
		nativeOwnershipCovUID, nativeOwnershipCovGID,
	))
}

// TestGeneratedNativeIdentityTraversalUsesTheApplicableModeClass proves
// traversability is decided by the single mode class the kernel would apply —
// owner, then group, then other — and never by a union of them. Reading the
// wrong class would accept a path the dropped identity cannot enter, or refuse
// one it can.
func TestGeneratedNativeIdentityTraversalUsesTheApplicableModeClass(t *testing.T) {
	const (
		uid = uint32(65534)
		gid = uint32(65535)
	)

	for _, testCase := range []struct {
		name string
		stat unix.Stat_t
		want bool
	}{
		{name: "owner execute", stat: unix.Stat_t{Uid: uid, Mode: 0o100}, want: true},
		{name: "owner without execute ignores group", stat: unix.Stat_t{Uid: uid, Gid: gid, Mode: 0o011}},
		{name: "group execute", stat: unix.Stat_t{Gid: gid, Mode: 0o010}, want: true},
		{name: "group without execute ignores other", stat: unix.Stat_t{Gid: gid, Mode: 0o101}},
		{name: "other execute", stat: unix.Stat_t{Mode: 0o001}, want: true},
		{name: "other without execute", stat: unix.Stat_t{Mode: 0o110}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, nativeIdentityCanTraverse(testCase.stat, uid, gid))
		})
	}
}

// TestGeneratedNativeTraversalValidatesTheFilesystemRootFirst proves the walk
// proves "/" itself before it opens a single component. A compromised
// filesystem root must not be walked through on the way to a trusted leaf, and
// the refusal has to arrive before any component descriptor exists.
func TestGeneratedNativeTraversalValidatesTheFilesystemRootFirst(t *testing.T) {
	nativeOwnershipCovRequireRoot(t)

	baseline := nativeOwnershipFstat
	calls := 0
	nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
		calls++
		if err := baseline(fd, stat); err != nil {
			return err
		}
		stat.Uid = nativeOwnershipCovUID

		return nil
	}

	t.Cleanup(func() { nativeOwnershipFstat = baseline })

	directory, err := openGeneratedNativeDirectory(
		"/etc/ssl/private", 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID,
	)
	require.Nil(t, directory)
	require.ErrorContains(t, err, "generated native path ancestry is not a trusted directory")
	require.Equal(t, 1, calls, "the walk opened a component past the refused filesystem root")
}

// TestGeneratedNativeTraversalWalksTheFilesystemRootAsItsOwnTarget proves a
// generated root of exactly "/" is answered with the already-open filesystem
// root instead of being fed to openat as an empty component, which the kernel
// would refuse. The staged read only supplies the 0700 metadata a generated root
// must have; the descriptor handed back is the real filesystem root.
func TestGeneratedNativeTraversalWalksTheFilesystemRootAsItsOwnTarget(t *testing.T) {
	nativeOwnershipCovRequireRoot(t)
	nativeOwnershipCovFaultFstat(t, func(calls int, stat *unix.Stat_t) error {
		if calls == 1 {
			stat.Mode = unix.S_IFDIR | 0o700
		}

		return nil
	})

	directory, err := openGeneratedNativeDirectory("/", 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	var opened, filesystemRoot unix.Stat_t
	require.NoError(t, unix.Fstat(int(directory.Fd()), &opened))
	require.NoError(t, unix.Stat("/", &filesystemRoot))
	require.Equal(t, filesystemRoot.Ino, opened.Ino)
	require.Equal(t, filesystemRoot.Dev, opened.Dev)
}

// TestGeneratedNativeTraversalRefusesAnUntraversableAncestor proves the walk
// refuses at the component that fails rather than at the leaf, so a generated
// root whose parent the dropped identity could never have entered is never
// opened at all.
func TestGeneratedNativeTraversalRefusesAnUntraversableAncestor(t *testing.T) {
	native := nativeOwnershipCovRoot(t)
	parent := filepath.Dir(native)
	require.NoError(t, os.Chmod(parent, 0o700))

	err := handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation())
	require.ErrorContains(t, err, "ancestry is not traversable by the target identity")

	nativeOwnershipCovRequireTrusted(t, parent, native)
}

// TestGeneratedNativeTraversalFailsClosedOnKernelFaults proves every descriptor
// syscall the walk depends on aborts it. A walk that swallowed any of these
// would hand back a descriptor whose ancestry it never actually proved.
func TestGeneratedNativeTraversalFailsClosedOnKernelFaults(t *testing.T) {
	nativeOwnershipCovRequireRoot(t)

	t.Run("filesystem root unopenable", func(t *testing.T) {
		previous := nativeOwnershipOpenFilesystemRoot
		nativeOwnershipOpenFilesystemRoot = func() (int, error) { return -1, unix.EMFILE }

		t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

		directory, err := openGeneratedNativeDirectory("/etc", 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID)
		require.ErrorIs(t, err, unix.EMFILE)
		require.Nil(t, directory)
	})

	t.Run("filesystem root unstattable", func(t *testing.T) {
		previous := nativeOwnershipFstat
		nativeOwnershipFstat = func(int, *unix.Stat_t) error { return unix.EIO }

		t.Cleanup(func() { nativeOwnershipFstat = previous })

		directory, err := openGeneratedNativeDirectory("/etc", 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID)
		require.ErrorIs(t, err, unix.EIO)
		require.Nil(t, directory)
	})

	t.Run("component unstattable", func(t *testing.T) {
		baseline := nativeOwnershipFstat
		calls := 0
		nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
			calls++
			if calls == 1 {
				return baseline(fd, stat)
			}

			return unix.EIO
		}

		t.Cleanup(func() { nativeOwnershipFstat = baseline })

		directory, err := openGeneratedNativeDirectory("/etc", 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID)
		require.ErrorIs(t, err, unix.EIO)
		require.Nil(t, directory)
		require.Equal(t, 2, calls, "the walk statted past the faulted component")
	})

	t.Run("parent descriptor unreleasable", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)

		previous := nativeOwnershipClose
		nativeOwnershipClose = func(fd int) error {
			_ = previous(fd)

			return unix.EIO
		}

		t.Cleanup(func() { nativeOwnershipClose = previous })

		require.ErrorIs(
			t, handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation()), unix.EIO,
		)
		nativeOwnershipCovRequireTrusted(t, native)
	})
}

// TestGeneratedNativeInodeRefusalsAreStated proves the pre-chown revalidation
// catches every way an inode behind an accepted descriptor can stop being the
// trusted inode the walk approved. Each refusal is asserted against the real
// tree: the generated root must still be trusted-owned afterwards, because a
// swallowed refusal would have handed it over.
func TestGeneratedNativeInodeRefusalsAreStated(t *testing.T) {
	isolation := nativeOwnershipCovIsolation()

	t.Run("descriptor stops answering", func(t *testing.T) {
		nativeOwnershipCovRequireRoot(t)

		closed, err := os.Open(os.DevNull)
		require.NoError(t, err)
		require.NoError(t, closed.Close())

		require.ErrorIs(
			t,
			handoffGeneratedNativeDirectory(closed, 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID),
			unix.EBADF,
		)
	})

	t.Run("entry owner already changed", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))
		require.NoError(t, os.Chown(seed, 4242, 4242))

		err := handoffGeneratedNativeTree(native, isolation)
		require.ErrorContains(t, err, "generated native inode owner changed to uid=4242 gid=4242")

		nativeOwnershipCovRequireTrusted(t, native)
	})

	t.Run("entry has a second link", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))
		require.NoError(t, os.Link(seed, filepath.Join(native, "alias")))

		err := handoffGeneratedNativeTree(native, isolation)
		require.ErrorContains(t, err, "generated native file has 2 links")

		nativeOwnershipCovRequireTrusted(t, native, seed)
	})

	t.Run("nested directory mode is unsafe", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		nested := filepath.Join(native, "nested")
		require.NoError(t, os.Mkdir(nested, 0o750))

		err := handoffGeneratedNativeTree(native, isolation)
		require.ErrorContains(t, err, "generated native directory mode 0750 is unsafe")

		nativeOwnershipCovRequireTrusted(t, native, nested)
	})

	t.Run("file mode is unsafe", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o640))

		err := handoffGeneratedNativeTree(native, isolation)
		require.ErrorContains(t, err, "generated native file mode 0640 is unsafe")

		nativeOwnershipCovRequireTrusted(t, native, seed)
	})

	t.Run("inode type changed after classification", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

		// The entry is classified by one stat and revalidated by another. Let
		// the classification see a regular file and make the revalidation
		// disagree, so the revalidation has to be load-bearing rather than a
		// restatement of the classification.
		baseline := nativeOwnershipFstat
		regular := 0
		nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
			if err := baseline(fd, stat); err != nil {
				return err
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFREG {
				regular++
				if regular > 1 {
					stat.Mode = unix.S_IFDIR | stat.Mode&0o7777
				}
			}

			return nil
		}

		t.Cleanup(func() { nativeOwnershipFstat = baseline })

		err := handoffGeneratedNativeTree(native, isolation)
		require.ErrorContains(t, err, "generated native inode type changed")

		nativeOwnershipCovRequireTrusted(t, native, seed)
	})
}

// TestGeneratedNativeDirectoryHandoffRefusesEntriesItCannotAccount proves the
// directory leg never chowns a root whose contents it could not walk. A
// directory it cannot enumerate, an entry it cannot open, and an entry that is
// neither a directory nor a regular file all abort before any ownership moves.
func TestGeneratedNativeDirectoryHandoffRefusesEntriesItCannotAccount(t *testing.T) {
	t.Run("directory cannot be enumerated", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

		directory := nativeOwnershipCovPathDescriptor(t, native, true)
		require.Error(t, handoffGeneratedNativeDirectory(
			directory, 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID,
		))

		nativeOwnershipCovRequireTrusted(t, native, seed)
	})

	t.Run("entry cannot be opened", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		require.NoError(t, os.Symlink("/etc/shadow", filepath.Join(native, "escape")))

		err := handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation())
		require.ErrorContains(t, err, `open generated native entry "escape"`)
		require.ErrorIs(t, err, unix.ELOOP)

		nativeOwnershipCovRequireTrusted(t, native, filepath.Join(native, "escape"))
	})

	t.Run("entry descriptor stops answering before classification", func(t *testing.T) {
		nativeOwnershipCovRequireRoot(t)

		entry, err := os.Open(os.DevNull)
		require.NoError(t, err)
		require.NoError(t, entry.Close())

		// A zero-valued stat would classify as an unsupported type, hiding the
		// fault behind a refusal that names the wrong reason.
		require.ErrorIs(
			t,
			handoffGeneratedNativeEntry(entry, 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID),
			unix.EBADF,
		)
	})

	t.Run("entry is neither a directory nor a regular file", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		fifo := filepath.Join(native, "channel")
		require.NoError(t, unix.Mkfifo(fifo, 0o600))

		err := handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation())
		require.ErrorContains(t, err, "generated native inode has unsupported type")

		nativeOwnershipCovRequireTrusted(t, native, fifo)
	})
}

// TestGeneratedNativeChownIsProvenRatherThanAssumed proves the handoff refuses
// unless it can re-read the inode and see the transfer it just asked for. A
// chown the kernel reports as succeeding but does not reflect back would
// otherwise be published as a completed handoff.
func TestGeneratedNativeChownIsProvenRatherThanAssumed(t *testing.T) {
	t.Run("chown refused", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

		entry := nativeOwnershipCovPathDescriptor(t, seed, false)
		require.ErrorIs(
			t,
			handoffGeneratedNativeEntry(entry, 0, 0, nativeOwnershipCovUID, nativeOwnershipCovGID),
			unix.EBADF,
		)

		nativeOwnershipCovRequireTrusted(t, seed)
	})

	t.Run("descriptor stops answering after chown", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		require.NoError(t, os.WriteFile(filepath.Join(native, "seed"), []byte("x"), 0o600))

		baseline := nativeOwnershipFstat
		nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
			if err := baseline(fd, stat); err != nil {
				return err
			}
			if stat.Uid == nativeOwnershipCovUID {
				return unix.EIO
			}

			return nil
		}

		t.Cleanup(func() { nativeOwnershipFstat = baseline })

		require.ErrorIs(
			t, handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation()), unix.EIO,
		)
	})

	t.Run("descriptor does not reflect the transfer", func(t *testing.T) {
		native := nativeOwnershipCovRoot(t)
		seed := filepath.Join(native, "seed")
		require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

		nativeOwnershipCovFaultFstat(t, func(_ int, stat *unix.Stat_t) error {
			if stat.Uid == nativeOwnershipCovUID {
				stat.Uid = 0
			}

			return nil
		})

		err := handoffGeneratedNativeTree(native, nativeOwnershipCovIsolation())
		require.ErrorContains(t, err, "ownership handoff could not be proven")
	})
}

// TestEffectiveIdentityFailsClosedOnAnUnrepresentableKernelAnswer proves the
// effective-id helpers refuse rather than narrow. Every caller compares their
// result against an inode's 32-bit owner, so an answer outside that width must
// not be truncated into an id a real inode could carry — the truncation of an
// answer one past the 32-bit range is 0, which is root. Linux stores its ids in
// 32 bits and cannot produce such an answer, so it is staged through the seams
// the helpers read.
func TestEffectiveIdentityFailsClosedOnAnUnrepresentableKernelAnswer(t *testing.T) {
	realUID, realGID := effectiveUIDSource, effectiveGIDSource
	t.Cleanup(func() { effectiveUIDSource, effectiveGIDSource = realUID, realGID })

	effectiveUIDSource = func() int { return -1 }
	effectiveGIDSource = func() int { return -1 }
	require.Equal(t, uint32(math.MaxUint32), effectiveUID())
	require.Equal(t, uint32(math.MaxUint32), effectiveGID())

	effectiveUIDSource = func() int { return math.MaxUint32 + 1 }
	effectiveGIDSource = func() int { return math.MaxUint32 + 1 }

	uid, gid := effectiveUID(), effectiveGID()
	require.Equal(t, uint32(math.MaxUint32), uid)
	require.Equal(t, uint32(math.MaxUint32), gid)
	require.NotZero(t, uid, "narrowing this answer would have claimed root")
	require.NotZero(t, gid, "narrowing this answer would have claimed the root group")

	effectiveUIDSource = func() int { return 65534 }
	effectiveGIDSource = func() int { return 65533 }
	require.Equal(t, uint32(65534), effectiveUID())
	require.Equal(t, uint32(65533), effectiveGID())
}
