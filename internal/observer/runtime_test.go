package observer

import (
	"context"
	"testing"
	"time"
)

func TestNilRuntimeObserverIsSafe(t *testing.T) {
	var observe *Observer
	observe.RecordRuntimeResourceAdmission(context.Background(), "adapter_scratch_root", "session", "admitted")
	observe.AddRuntimeResource(context.Background(), "adapter_scratch_root", 1)
	observe.ObserveRuntimeStartupStage(context.Background(), "session", "spawn", time.Millisecond, nil)
}
