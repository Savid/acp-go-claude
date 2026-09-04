//go:build windows

package claude

import (
	"os"
	"path/filepath"
)

// The spellings the ordinary-process residual tests need from the host. They
// differ per platform in name, extension, and search-path separator; the
// behavior they prove does not.
const (
	// residualSearchExecutable is resolved through the search path below. It is
	// named without its extension on purpose: PATHEXT resolution is the part of
	// the search Windows adds.
	residualSearchExecutable = "cmd"

	// residualExecutableSuffix is what a file needs before the platform will try
	// to run it. Windows decides that by name, never by mode.
	residualExecutableSuffix = ".exe"
)

var (
	residualSystemDir = filepath.Join(residualSystemRoot(), "System32")

	// residualSearchPath puts one empty entry ahead of the directory holding
	// that program, so the search's empty-entry branch is exercised too.
	residualSearchPath = "PATH=;" + residualSystemDir

	// residualResolvedExecutable is what the search must resolve to.
	residualResolvedExecutable = filepath.Join(residualSystemDir, "cmd.exe")

	// residualExitCommand exits immediately.
	residualExitCommand = []string{filepath.Join(residualSystemDir, "cmd.exe"), "/c", "exit"}

	// residualLingeringCommand stays alive until it is revoked.
	residualLingeringCommand = []string{filepath.Join(residualSystemDir, "ping.exe"), "-n", "61", "127.0.0.1"}
)

func residualSystemRoot() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return root
	}

	return `C:\Windows`
}
