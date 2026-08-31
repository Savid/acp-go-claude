package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

func startOrdinaryNative(executable string, arguments []string, environment []string, cwd string) (NativeProcess, error) {
	resolved, err := resolveOrdinaryExecutable(executable, environment)
	if err != nil {
		return nil, err
	}

	command := exec.Command(resolved, arguments...) // #nosec G702 -- ordinary mode intentionally uses the statically resolved operator-selected executable.
	command.Dir = cwd
	command.Env = environment

	childStdin, stdin, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create native stdin: %w", err)
	}

	stdout, childStdout, err := os.Pipe()
	if err != nil {
		_ = childStdin.Close()
		_ = stdin.Close()

		return nil, fmt.Errorf("create native stdout: %w", err)
	}

	stderr, childStderr, err := os.Pipe()
	if err != nil {
		_ = childStdin.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()

		return nil, fmt.Errorf("create native stderr: %w", err)
	}

	command.Stdin = childStdin
	command.Stdout = childStdout
	command.Stderr = childStderr

	if err := command.Start(); err != nil {
		_ = childStdin.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()
		_ = stderr.Close()
		_ = childStderr.Close()

		return nil, fmt.Errorf("start native process: %w", err)
	}

	_ = childStdin.Close()
	_ = childStdout.Close()
	_ = childStderr.Close()

	process := &ordinaryProcess{command: command, stdin: stdin, stdout: stdout, stderr: stderr, waitDone: make(chan struct{}), revokeDone: make(chan struct{})}
	process.collectOnce.Do(func() { go process.collect() })

	return process, nil
}

var ordinaryGetwd = os.Getwd

func resolveOrdinaryExecutable(name string, environment []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("executable path is empty")
	}

	if filepath.Base(name) != name {
		candidate, err := ordinaryAnchoredPath(name)
		if err != nil {
			return "", err
		}

		resolved, err := ordinaryExecutableCandidate(candidate, environment)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", name, err)
		}

		return resolved, nil
	}

	for _, directory := range filepath.SplitList(ordinaryEnvironmentValue(environment, envSearchPath)) {
		if directory == "" {
			continue
		}

		candidate, err := ordinaryAnchoredPath(filepath.Join(directory, name))
		if err != nil {
			return "", err
		}

		if resolved, candidateErr := ordinaryExecutableCandidate(candidate, environment); candidateErr == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", name, exec.ErrNotFound)
}

func ordinaryAnchoredPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}

	directory, err := ordinaryGetwd()
	if err != nil {
		return "", fmt.Errorf("anchor executable %q to the working directory: %w", path, err)
	}

	return filepath.Join(directory, path), nil
}

func ordinaryEnvironmentValue(environment []string, key string) string {
	value := ""
	identity := EnvironmentKey(key)

	for _, entry := range environment {
		candidate, candidateValue, ok := strings.Cut(entry, "=")
		if ok && EnvironmentKey(candidate) == identity {
			value = candidateValue
		}
	}

	return value
}

type ordinaryProcess struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	waitDone    chan struct{}
	waitResult  NativeResult
	waitErr     error
	revokeWon   bool
	resultMu    sync.Mutex
	collectOnce sync.Once
	revokeOnce  sync.Once
	revokeDone  chan struct{}
	revokeErr   error
}

func (p *ordinaryProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *ordinaryProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *ordinaryProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *ordinaryProcess) collect() {
	err := p.command.Wait()
	p.resultMu.Lock()
	p.waitResult = ordinaryNativeResult(p.command.ProcessState, p.revokeWon)

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		err = nil
	}

	p.waitErr = err
	p.resultMu.Unlock()
	close(p.waitDone)
}

func (p *ordinaryProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.collectOnce.Do(func() { go p.collect() })

	select {
	case <-p.waitDone:
		p.resultMu.Lock()
		defer p.resultMu.Unlock()

		return p.waitResult, p.waitErr
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (p *ordinaryProcess) Revoke(ctx context.Context) error {
	p.revokeOnce.Do(func() {
		go func() {
			defer close(p.revokeDone)

			select {
			case <-p.waitDone:
				return
			default:
			}

			_ = p.stdin.Close()
			p.resultMu.Lock()

			killErr := p.command.Process.Kill()
			if killErr != nil {
				p.resultMu.Unlock()

				if !errors.Is(killErr, os.ErrProcessDone) {
					p.revokeErr = killErr

					return
				}
			} else {
				p.revokeWon = true
				p.resultMu.Unlock()
			}

			p.collectOnce.Do(func() { go p.collect() })
			<-p.waitDone
		}()
	})

	select {
	case <-p.revokeDone:
		return p.revokeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

var ordinaryEnviron = os.Environ

func OrdinaryEnvironment() map[string]string {
	base := map[string]string{}

	for _, entry := range ordinaryEnviron() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(value, '\x00') || EnvironmentKey(key) == EnvironmentKey(envClaudeCodeNested) || privateAdapterEnvName(key) || authScrubbedEnvKey(key) {
			continue
		}

		base[key] = value
	}

	return base
}
