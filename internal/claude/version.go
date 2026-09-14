package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// MinimumVersion is the lowest native CLI version the adapter accepts.
const MinimumVersion = "2.0.0"

// CheckMinimumVersion fails when version sorts below minimum.
func CheckMinimumVersion(version string, minimum string) error {
	comparison, err := compareVersions(version, minimum)
	if err != nil {
		return err
	}

	if comparison < 0 {
		return fmt.Errorf("claude version %s is below the minimum supported version %s", version, minimum)
	}

	return nil
}

func compareVersions(left string, right string) (int, error) {
	leftParts, err := versionParts(left)
	if err != nil {
		return 0, err
	}

	rightParts, err := versionParts(right)
	if err != nil {
		return 0, err
	}

	for index := range max(len(leftParts), len(rightParts)) {
		leftValue := 0
		if index < len(leftParts) {
			leftValue = leftParts[index]
		}

		rightValue := 0
		if index < len(rightParts) {
			rightValue = rightParts[index]
		}

		if leftValue != rightValue {
			if leftValue < rightValue {
				return -1, nil
			}

			return 1, nil
		}
	}

	return 0, nil
}

func versionParts(version string) ([]int, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if release, _, found := strings.Cut(trimmed, "-"); found {
		trimmed = release
	}

	if trimmed == "" {
		return nil, fmt.Errorf("invalid version %q", version)
	}

	segments := strings.Split(trimmed, ".")
	parts := make([]int, 0, len(segments))

	for _, segment := range segments {
		value, err := strconv.Atoi(segment)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("invalid version %q", version)
		}

		parts = append(parts, value)
	}

	return parts, nil
}

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
