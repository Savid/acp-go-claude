package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

func startOrdinaryNative(executable string, arguments []string, environment []string, cwd string) (NativeProcess, error) {
	command := exec.Command(executable, arguments...) // #nosec G702 -- ordinary mode intentionally uses the operator-selected executable.
	command.Dir = cwd
	command.Env = environment

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create native stdin: %w", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, fmt.Errorf("create native stdout: %w", err)
	}

	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, fmt.Errorf("create native stderr: %w", err)
	}

	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		return nil, fmt.Errorf("start native process: %w", err)
	}

	process := &ordinaryProcess{command: command, stdin: stdin, stdout: stdout, stderr: stderr, waitDone: make(chan struct{}), revokeDone: make(chan struct{})}

	return process, nil
}

type ordinaryProcess struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	waitDone    chan struct{}
	waitResult  NativeResult
	waitErr     error
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
	p.waitResult.ExitCode = p.command.ProcessState.ExitCode()

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
			if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				p.revokeErr = err

				return
			}

			p.collectOnce.Do(func() { go p.collect() })
			<-p.waitDone
			p.resultMu.Lock()
			p.waitResult.Revoked = true
			p.resultMu.Unlock()
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
