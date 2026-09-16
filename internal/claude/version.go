package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// MinimumVersion is the lowest native CLI version the adapter accepts.
const MinimumVersion = "2.0.0"

// ProbeVersion reads the installed CLI's version without starting a session.
func ProbeVersion(ctx context.Context, executable string, environment []string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Env = environment

	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("probe claude version: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", errors.New("empty claude version")
	}

	return fields[0], nil
}
