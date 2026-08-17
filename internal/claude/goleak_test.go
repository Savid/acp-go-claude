package claude

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Process-transport tests use fake CLIs that do not implement --version;
	// disable the version probe here and cover it directly in command_test.go.
	claudeVersionProbe = func(context.Context, Executable, Options) error { return nil }

	goleak.VerifyTestMain(m)
}
