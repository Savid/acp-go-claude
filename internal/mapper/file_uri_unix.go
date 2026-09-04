//go:build !windows

package mapper

// FileURIHostPath converts the path component of a file URI into a host path.
// A POSIX file URI already carries the host path verbatim, so there is nothing
// to undo here.
func FileURIHostPath(path string) string { return path }
