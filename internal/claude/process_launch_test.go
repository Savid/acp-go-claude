package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestProcessTreeCommandDescriptors(t *testing.T) {
	if err := (*processTreeCommand)(nil).releaseStartGate(); err != nil {
		t.Fatal(err)
	}
	(*processTreeCommand)(nil).releaseInherited()
	(*processTreeCommand)(nil).abortStartGate()
	(*processTreeCommand)(nil).close()

	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateRead.Close()
	command := &processTreeCommand{startGate: gateWrite}
	releaseErr := command.releaseStartGate()
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	payload, err := io.ReadAll(gateRead)
	if err != nil || string(payload) != "\x01" || command.startGate != nil {
		t.Fatalf("gate payload=%q err=%v gate=%v", payload, err, command.startGate)
	}
	command.abortStartGate()

	_, closedWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := closedWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (&processTreeCommand{startGate: closedWrite}).releaseStartGate(); err == nil {
		t.Fatal("closed gate release error = nil")
	}

	newRead := func() *os.File {
		read, write, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		_ = write.Close()

		return read
	}
	inherited := newRead()
	command = &processTreeCommand{
		inherited: []*os.File{inherited},
		startGate: newRead(),
		control:   newRead(),
		ready:     newRead(),
		proof:     newRead(),
	}
	command.close()
	if command.inherited != nil || command.startGate != nil || command.control != nil || command.ready != nil || command.proof != nil {
		t.Fatalf("closed command = %#v", command)
	}
	command.close()
}

func TestPausedCommandWait(t *testing.T) {
	want := errors.New("wait")
	waiter, begin := startPausedCommandWait(func() error { return want })
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err, completed := waiter.await(ctx); completed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paused await error=%v completed=%v", err, completed)
	}
	begin()
	begin()
	if err, completed := waiter.await(t.Context()); !completed || !errors.Is(err, want) {
		t.Fatalf("completed await error=%v completed=%v", err, completed)
	}

	waiter, begin = startPausedCommandWait(nil)
	begin()
	if err, completed := waiter.await(t.Context()); err != nil || !completed {
		t.Fatalf("nil wait error=%v completed=%v", err, completed)
	}
	if err, completed := (*commandWait)(nil).await(t.Context()); err != nil || !completed {
		t.Fatalf("nil waiter error=%v completed=%v", err, completed)
	}
}
