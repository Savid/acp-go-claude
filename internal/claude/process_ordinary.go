package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ordinaryEnviron = os.Environ

// OrdinaryEnvironment captures the sanitized ambient environment every native
// process runs with when no explicit ProcessIsolation was configured. Omission
// selects ordinary same-identity execution, so there is no policy to read a
// replacement base from and the adapter's own environment is the base. The
// capture happens once per call, so every launch built from one result sees the
// same values however the ambient environment changes later.
func OrdinaryEnvironment() map[string]string {
	base := map[string]string{}

	for _, entry := range ordinaryEnviron() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(value, '\x00') ||
			key == envClaudeCodeNested || strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) ||
			authScrubbedEnvKey(key) {
			continue
		}

		base[key] = value
	}

	return base
}

// resolveOrdinaryExecutable resolves a native executable the way the operator's
// own shell would. Ordinary execution inherits the ambient search path rather
// than a curated policy one, so the hardened absolute-entry rule does not apply
// here; the platform's own rules — including Windows executable extensions —
// decide what is runnable.
func resolveOrdinaryExecutable(name string, env []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("executable path is empty")
	}

	if filepath.Base(name) != name {
		resolved, err := ordinaryLookPath(name)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", name, err)
		}

		return resolved, nil
	}

	for _, directory := range filepath.SplitList(environmentMap(env)[envSearchPath]) {
		if directory == "" {
			continue
		}

		if resolved, err := ordinaryLookPath(filepath.Join(directory, name)); err == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", name, exec.ErrNotFound)
}

var ordinaryLookPath = exec.LookPath

// resolveLaunchExecutable picks the resolution rules the selected launch mode
// actually has. A hardened policy owns its whole PATH and is held to absolute
// entries; ordinary execution reads the operator's own PATH and is not.
func resolveLaunchExecutable(options Options, name string, env []string) (string, error) {
	if options.ProcessIsolation == nil {
		return resolveOrdinaryExecutable(name, env)
	}

	return resolveProcessExecutable(name, env)
}

// prepareOrdinaryLaunch selects the directly-owned process boundary. It arms no
// identity guardian, no standalone disposition and no privileged supervisor: the
// adapter starts the native command itself, as the identity it already runs as.
// The caller's scratch generation is left alone: it is a directory the caller
// owns and finishes, never a containment registry this boundary reports to.
func prepareOrdinaryLaunch(cmd *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
	if cmd == nil {
		return nil, errors.New("claude ordinary launch command is unavailable")
	}

	return &processTreeCommand{cmd: cmd, ordinary: true}, nil
}

// ordinaryBoundary is the boundary an omitted ProcessIsolation selects: the
// process the adapter started and the process group it placed that process in.
// It publishes no descendant inventory and proves nothing about work that left
// the group, so its completion means only that the directly owned boundary
// finished.
type ordinaryBoundary struct {
	cmd    *exec.Cmd
	waiter *commandWait

	once sync.Once
	err  error
}

func startOrdinaryBoundary(launch *processTreeCommand) (*ordinaryBoundary, error) {
	if launch == nil || launch.cmd == nil {
		return nil, errors.New("claude ordinary launch is unavailable")
	}

	if err := launch.cmd.Start(); err != nil {
		launch.close()

		return nil, err
	}

	launch.releaseInherited()

	waiter, begin := startPausedCommandWait(launch.cmd.Wait)
	begin()

	return &ordinaryBoundary{cmd: launch.cmd, waiter: waiter}, nil
}

// complete ends the directly owned boundary: the process group is asked to
// stop, killed if it does not, and the direct child is reaped. It is the
// ordinary counterpart of an authoritative quiescence proof and deliberately
// claims nothing about a descendant that left the group.
func (b *ordinaryBoundary) complete(timeout time.Duration) error {
	if b == nil {
		return errors.New("claude ordinary boundary is unavailable")
	}

	b.once.Do(func() {
		if _, err := processTerminate(b.cmd); err != nil {
			b.err = fmt.Errorf("%w: terminate Claude process: %v", ErrProcessContainmentIncomplete, err)

			return
		}

		if b.awaitChild(timeout) {
			return
		}

		if _, err := processKill(b.cmd); err != nil {
			b.err = fmt.Errorf("%w: kill Claude process: %v", ErrProcessContainmentIncomplete, err)

			return
		}

		if !b.awaitChild(timeout) {
			b.err = fmt.Errorf("%w: direct Claude child was not reaped", ErrProcessContainmentIncomplete)
		}
	})

	return b.err
}

func (b *ordinaryBoundary) awaitChild(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, completed := b.waiter.await(ctx)

	return completed
}

// wait reports the direct child's own exit. The boundary started the sole
// waiter when it started the child, so this collects that result instead of
// reaping a second time.
func (b *ordinaryBoundary) wait() error {
	waitErr, _ := b.waiter.await(context.Background())

	return waitErr
}

// ordinaryWaiter exposes the sole direct-child reap to callers that observe the
// child's exit without owning the boundary.
func (b *ordinaryBoundary) ordinaryWaiter() *commandWait {
	if b == nil {
		return nil
	}

	return b.waiter
}
