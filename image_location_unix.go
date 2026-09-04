//go:build !windows

package claudeacp

// imageLocationScheme reports the URI scheme a local image location carries.
// A POSIX host path can never be mistaken for one, so the parsed scheme stands.
func imageLocationScheme(scheme string, _ string) string { return scheme }
