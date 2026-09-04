//go:build windows

package claudeacp

import "path/filepath"

// imageLocationScheme reports the URI scheme a local image location carries.
// A Windows path opens with a drive letter and a colon, which url.Parse reads as
// a one-letter scheme, so C:\images\a.png would be refused as an unsupported
// scheme rather than resolved as the host path it is. A one-letter scheme on a
// location that carries a volume name is that volume, never a scheme.
func imageLocationScheme(scheme string, location string) string {
	if len(scheme) == 1 && filepath.VolumeName(location) != "" {
		return ""
	}

	return scheme
}
