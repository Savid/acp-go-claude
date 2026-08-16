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
// to the PATH itself; the platform's own rules — including Windows executable
// extensions — decide what is runnable. Every candidate is anchored to the
// adapter's own working directory before it is examined, so what this answers
// is the file the launch will run and not a name resolved a second time.
func resolveOrdinaryExecutable(name string, env []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("executable path is empty")
	}

	if filepath.Base(name) != name {
		anchored, err := ordinaryAnchoredPath(name)
		if err != nil {
			return "", err
		}

		resolved, err := ordinaryLookPath(anchored, env)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", name, err)
		}

		return resolved, nil
	}

	for _, directory := range filepath.SplitList(ordinaryEnvironmentValue(env, envSearchPath)) {
		if directory == "" {
			continue
		}

		// filepath.Join cleans, so a "." entry would otherwise yield a bare name
		// and send the platform candidate check back to the adapter's ambient
		// PATH instead of the search path this launch was built with.
		anchored, err := ordinaryAnchoredPath(filepath.Join(directory, name))
		if err != nil {
			return "", err
		}

		if resolved, err := ordinaryLookPath(anchored, env); err == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", name, exec.ErrNotFound)
}

// ordinaryAnchoredPath anchors a candidate to the directory the adapter itself
// runs in. The launch replaces the child's working directory with the session's,
// so a relative candidate would be examined here and executed there.
func ordinaryAnchoredPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}

	directory, err := processGetwd()
	if err != nil {
		return "", fmt.Errorf("anchor executable %q to the working directory: %w", path, err)
	}

	return filepath.Join(directory, path), nil
}

var ordinaryLookPath = ordinaryExecutableCandidate

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
	begin  func()

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

	// The reap is created here and deliberately left paused. The command this
	// boundary owns is the one the caller took its stdout, stderr and stdin
	// pipes from, and os/exec closes the parent ends of those pipes the moment
	// Wait returns. A reap started with the child would therefore race the
	// caller's own reader: a Claude turn that wrote its final result line and
	// exited would be reported as a truncated read instead. Whoever owns the
	// wait begins it.
	waiter, begin := startPausedCommandWait(launch.cmd.Wait)

	return &ordinaryBoundary{cmd: launch.cmd, waiter: waiter, begin: begin}, nil
}

// beginWait starts the boundary's sole reap. Every caller that owns or observes
// the child's exit reaches it, and startPausedCommandWait makes the start
// idempotent, so the child is still waited on exactly once however many of them
// run.
func (b *ordinaryBoundary) beginWait() {
	if b.begin == nil {
		return
	}

	b.begin()
}

// complete ends the directly owned boundary: the reap is started, the process
// group is asked to stop, killed if it does not, and the direct child is
// reaped. It is what the ordinary boundary has instead of an authoritative
// quiescence proof and deliberately claims nothing about a descendant that left
// the group.
func (b *ordinaryBoundary) complete(timeout time.Duration) error {
	if b == nil {
		return errors.New("claude ordinary boundary is unavailable")
	}

	b.once.Do(func() {
		// A child that never exits on its own must still be terminated and
		// collected here, so the paused reap is started before the ladder runs
		// rather than left waiting for a reader that already gave up.
		b.beginWait()

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

// wait reports the direct child's own exit. This is the point that owns the
// wait for a caller that read the child's output to EOF first, so it starts the
// boundary's single paused reap and then collects its result rather than
// reaping a second time.
func (b *ordinaryBoundary) wait() error {
	if b == nil {
		return errors.New("claude ordinary boundary is unavailable")
	}

	b.beginWait()

	waitErr, _ := b.waiter.await(context.Background())

	return waitErr
}

// observeExit hands the boundary's single reap to a caller that watches for the
// child's exit without owning the boundary. Such a caller polls the reap for an
// answer, so the observation has to be running for it to have one; the boundary
// still reaps exactly once.
func (b *ordinaryBoundary) observeExit() *commandWait {
	if b == nil {
		return nil
	}

	b.beginWait()

	return b.waiter
}
