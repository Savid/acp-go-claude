package claude

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestObserveStartupStage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	(&Client{}).observeStartupStage(ctx, "spawn", time.Now(), nil)

	wantErr := errors.New("spawn failed")
	called := false
	client := &Client{options: Options{ObserveStartupStage: func(gotCtx context.Context, stage string, elapsed time.Duration, err error) {
		called = true
		if gotCtx != ctx || stage != "spawn" || elapsed < 0 || !errors.Is(err, wantErr) {
			t.Fatalf("observation = (%v, %q, %v, %v)", gotCtx, stage, elapsed, err)
		}
	}}}
	client.observeStartupStage(ctx, "spawn", time.Now(), wantErr)
	if !called {
		t.Fatal("startup-stage callback was not called")
	}
}
