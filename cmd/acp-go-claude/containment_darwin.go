//go:build darwin

package main

import (
	"io"

	nativeclaude "github.com/savid/acp-go-claude/internal/claude"
)

var diagnoseContainment = func(scratchDir string, output io.Writer) error {
	return nativeclaude.DiagnoseDarwinContainment(scratchDir, output)
}

var cleanupContainment = func(scratchDir, runtimeID string, force bool, output io.Writer) error {
	return nativeclaude.CleanupDarwinContainment(scratchDir, runtimeID, force, output)
}
