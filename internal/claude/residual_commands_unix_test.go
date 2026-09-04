//go:build !windows

package claude

// The spellings the ordinary-process residual tests need from the host. They
// differ per platform in name, extension, and search-path separator; the
// behavior they prove does not.
const (
	// residualSearchExecutable is resolved through the search path below. POSIX
	// carries no executable extension, so the name is the whole file. sh is the
	// one program POSIX places at a fixed path on every host; true moves between
	// /bin and /usr/bin.
	residualSearchExecutable = "sh"

	// residualSearchPath puts one empty entry ahead of the directory holding
	// that program, so the search's empty-entry branch is exercised too.
	residualSearchPath = "PATH=:/bin"

	// residualResolvedExecutable is what the search must resolve to.
	residualResolvedExecutable = "/bin/sh"

	// residualExecutableSuffix is what a file needs before the platform will try
	// to run it. POSIX decides that by mode, never by name.
	residualExecutableSuffix = ""
)

var (
	// residualExitCommand exits immediately.
	residualExitCommand = []string{"/bin/sh", "-c", "exit 0"}

	// residualLingeringCommand stays alive until it is revoked.
	residualLingeringCommand = []string{"/bin/sh", "-c", "sleep 60"}
)
