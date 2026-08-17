package claude

import (
	"context"
	"errors"
	"sync"
)

type processLaunchOptions struct {
	DarwinBestEffort bool
	Generation       *DarwinGeneration
	Isolation        *ProcessIsolation
}

type commandWait struct {
	done chan struct{}
	err  error
}

func startPausedCommandWait(wait func() error) (*commandWait, func()) {
	state := &commandWait{done: make(chan struct{})}
	start := make(chan struct{})

	var once sync.Once

	go func() {
		<-start

		if wait != nil {
			state.err = wait()
		}

		close(state.done)
	}()

	return state, func() { once.Do(func() { close(start) }) }
}

func (wait *commandWait) await(ctx context.Context) (error, bool) {
	if wait == nil {
		return nil, true
	}

	select {
	case <-wait.done:
		return wait.err, true
	case <-ctx.Done():
		return ctx.Err(), false
	}
}

func (c *processTreeCommand) releaseInherited() {
	if c == nil {
		return
	}

	for _, file := range c.inherited {
		_ = file.Close()
	}

	c.inherited = nil
}

func (c *processTreeCommand) releaseStartGate() error {
	if c == nil || c.startGate == nil {
		return nil
	}

	gate := c.startGate
	c.startGate = nil
	_, writeErr := gate.Write([]byte{1})
	closeErr := gate.Close()

	return errors.Join(writeErr, closeErr)
}

func (c *processTreeCommand) abortStartGate() {
	if c == nil || c.startGate == nil {
		return
	}

	_ = c.startGate.Close()
	c.startGate = nil
}

func (c *processTreeCommand) close() {
	if c == nil {
		return
	}

	c.releaseInherited()
	c.abortStartGate()

	if c.control != nil {
		_ = c.control.Close()
		c.control = nil
	}

	if c.ready != nil {
		_ = c.ready.Close()
		c.ready = nil
	}

	if c.proof != nil {
		_ = c.proof.Close()
		c.proof = nil
	}
}
