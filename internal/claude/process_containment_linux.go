//go:build linux

package claude

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

type processContainment struct {
	mu             sync.Mutex
	processGroupID int
	control        *os.File
	proof          *os.File
	proofOnce      sync.Once
	proofErr       error
}

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	if launch == nil || launch.cmd == nil {
		return nil, errors.New("claude containment launch is unavailable")
	}

	if launch.cmd.SysProcAttr == nil || !launch.cmd.SysProcAttr.Setpgid {
		launch.close()

		return nil, errors.New("claude process supervisor group is not configured")
	}

	if err := launch.cmd.Start(); err != nil {
		launch.close()

		return nil, err
	}

	launch.releaseInherited()

	if err := awaitProcessTreeReady(launch); err != nil {
		if launch.control != nil {
			_ = launch.control.Close()
			launch.control = nil
		}

		proofErr := awaitProcessTreeProof(launch.proof, 5*time.Second)
		launch.proof = nil
		waitErr := launch.cmd.Wait()
		launch.close()

		if proofErr != nil {
			proofErr = fmt.Errorf("%w: %v", ErrProcessTreeUnproven, proofErr)
		}

		return nil, errors.Join(err, proofErr, waitErr)
	}

	tree := &processContainment{
		processGroupID: launch.cmd.Process.Pid,
		control:        launch.control,
		proof:          launch.proof,
	}
	launch.control = nil
	launch.proof = nil

	return tree, nil
}

func (c *processContainment) quiesce(timeout time.Duration) error {
	if c == nil || c.processGroupID <= 0 {
		return errors.New("claude process supervisor identity is unavailable")
	}

	c.proofOnce.Do(func() {
		c.mu.Lock()
		if c.control == nil {
			c.proofErr = errors.New("claude process supervisor control is unavailable")
			c.mu.Unlock()

			return
		}

		closeErr := c.control.Close()
		c.control = nil
		c.mu.Unlock()

		if closeErr != nil {
			c.proofErr = fmt.Errorf("close Claude process supervisor control: %w", closeErr)

			return
		}

		c.mu.Lock()
		proof := c.proof
		c.proof = nil
		c.mu.Unlock()

		if proofErr := awaitProcessTreeProof(proof, timeout); proofErr != nil {
			c.proofErr = fmt.Errorf("%w: %v", ErrProcessTreeUnproven, proofErr)

			return
		}

		c.proofErr = c.waitUntilEmpty(timeout)
	})

	return c.proofErr
}

func awaitProcessTreeProof(proof *os.File, timeout time.Duration) error {
	if proof == nil {
		return errors.New("claude process supervisor proof descriptor is unavailable")
	}

	defer func() { _ = proof.Close() }()

	if timeout <= 0 {
		timeout = time.Second
	}

	if err := proof.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("arm Claude process supervisor proof: %w", err)
	}

	line, err := bufio.NewReader(proof).ReadString('\n')
	if err != nil {
		return fmt.Errorf("await Claude process supervisor proof: %w", err)
	}

	if line != turnSupervisorProof {
		return fmt.Errorf("invalid Claude process supervisor proof %q", line)
	}

	return nil
}

func (c *processContainment) waitUntilEmpty(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Second
	}

	deadline := time.Now().Add(timeout)

	for {
		err := syscallKill(-c.processGroupID, 0)
		switch {
		case errors.Is(err, syscall.ESRCH):
			return nil
		case err != nil && !errors.Is(err, syscall.EPERM):
			return fmt.Errorf("probe Claude process supervisor %d: %w", c.processGroupID, err)
		case !time.Now().Before(deadline):
			return fmt.Errorf("claude process supervisor %d did not publish quiescence before deadline", c.processGroupID)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (c *processContainment) close() error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.control == nil {
		if c.proof != nil {
			err := c.proof.Close()
			c.proof = nil

			return err
		}

		return nil
	}

	err := c.control.Close()

	c.control = nil
	if c.proof != nil {
		err = errors.Join(err, c.proof.Close())
		c.proof = nil
	}

	return err
}

func (*processContainment) processSnapshot() (int, bool) { return 0, false }
