//go:build linux

package claude

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

type agentIdentityLock struct {
	fd int
}

func acquireLinuxAgentIdentityLock(uid uint32, control io.Reader) (io.Closer, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("agent identity lock requires a trusted root supervisor")
	}

	rootFD, err := openLinuxLockDirectory("/run", unix.AT_FDCWD, 0o755, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)

	acpGoFD, err := openLinuxLockDirectory("acp-go", rootFD, 0o700, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(acpGoFD)

	namespaceFD, err := openLinuxLockDirectory("agent-identities", acpGoFD, 0o700, true)
	if err != nil {
		return nil, err
	}

	name := strconv.FormatUint(uint64(uid), 10) + ".lock"
	fd, err := unix.Openat(namespaceFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	_ = unix.Close(namespaceFD)
	if err != nil {
		return nil, fmt.Errorf("open agent identity lock %q: %w", name, err)
	}

	if err := verifyLinuxLockFile(fd); err != nil {
		_ = unix.Close(fd)

		return nil, err
	}

	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &agentIdentityLock{fd: fd}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = unix.Close(fd)

			return nil, fmt.Errorf("acquire agent identity lock for uid %d: %w", uid, err)
		}
		if linuxControlClosed(control) {
			_ = unix.Close(fd)

			return nil, fmt.Errorf("%w: control closed while waiting for agent identity uid %d", ErrProcessContainmentIncomplete, uid)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func openLinuxLockDirectory(name string, parentFD int, mode uint32, create bool) (int, error) {
	if create {
		err := unix.Mkdirat(parentFD, name, mode)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("create agent identity lock directory %q: %w", name, err)
		}
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open agent identity lock directory %q: %w", name, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("stat agent identity lock directory %q: %w", name, err)
	}
	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("agent identity lock directory %q is not a root-owned protected directory", name)
	}
	if create && stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("agent identity lock directory %q must have mode 0700", name)
	}

	return fd, nil
}

func verifyLinuxLockFile(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat agent identity lock: %w", err)
	}
	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0o777 != 0o600 {
		return errors.New("agent identity lock is not a root-owned single-link regular file")
	}

	return nil
}

func linuxControlClosed(control io.Reader) bool {
	file, ok := control.(*os.File)
	if !ok {
		return false
	}

	poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLHUP | unix.POLLERR}} //nolint:gosec // inherited descriptors fit pollfd.
	count, err := unix.Poll(poll, 0)

	return err == nil && count > 0 && poll[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0
}

func (lock *agentIdentityLock) Close() error {
	if lock == nil || lock.fd < 0 {
		return nil
	}

	fd := lock.fd
	lock.fd = -1

	return errors.Join(unix.Flock(fd, unix.LOCK_UN), unix.Close(fd))
}
