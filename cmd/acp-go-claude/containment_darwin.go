//go:build darwin

package main

import (
	"io"

	nativeclaude "github.com/savid/acp-go-claude/internal/claude"
)

func diagnoseContainment(scratchDir string, output io.Writer) error {
	return nativeclaude.DiagnoseDarwinContainment(scratchDir, output)
}

func cleanupContainment(scratchDir, runtimeID string, force bool, output io.Writer) error {
	return nativeclaude.CleanupDarwinContainment(scratchDir, runtimeID, force, output)
}
