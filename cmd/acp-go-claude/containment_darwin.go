//go:build darwin

package main

import (
	nativeclaude "github.com/savid/acp-go-claude/internal/claude"
)

var diagnoseContainment = nativeclaude.DiagnoseDarwinContainment

var cleanupContainment = nativeclaude.CleanupDarwinContainment
