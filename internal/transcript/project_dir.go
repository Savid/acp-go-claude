package transcript

import (
	"strconv"
	"unicode/utf16"
)

// projectDirMaxUnits is the longest sanitized project-directory name Claude
// Code writes verbatim. A longer name is truncated to this many UTF-16 code
// units and given a hash suffix.
const projectDirMaxUnits = 200

// ProjectDirName returns the directory name Claude Code uses under
// <config>/projects for a canonical working directory.
//
// Claude replaces every non-alphanumeric UTF-16 code unit with "-" and, once
// the result is longer than projectDirMaxUnits, truncates it and appends a
// base-36 hash of the original path. Deriving the name without the truncation
// rule points reads and writes at a directory Claude never uses, so a
// materialized transcript for a deep working directory becomes invisible to
// `claude --resume`.
func ProjectDirName(path string) string {
	units := utf16.Encode([]rune(path))

	sanitized := make([]uint16, len(units))
	for index, unit := range units {
		if projectDirAlphanumeric(unit) {
			sanitized[index] = unit

			continue
		}

		sanitized[index] = '-'
	}

	if len(sanitized) == 0 {
		return "-"
	}

	if len(sanitized) <= projectDirMaxUnits {
		return string(utf16.Decode(sanitized))
	}

	return string(utf16.Decode(sanitized[:projectDirMaxUnits])) + "-" + projectDirHash(units)
}

func projectDirAlphanumeric(unit uint16) bool {
	return (unit >= 'a' && unit <= 'z') || (unit >= 'A' && unit <= 'Z') || (unit >= '0' && unit <= '9')
}

// projectDirHash reproduces Claude's 32-bit string hash over the untruncated
// path, rendered as an unsigned base-36 magnitude.
func projectDirHash(units []uint16) string {
	var hash int32
	for _, unit := range units {
		hash = hash*31 + int32(unit)
	}

	magnitude := int64(hash)
	if magnitude < 0 {
		magnitude = -magnitude
	}

	return strconv.FormatInt(magnitude, 36)
}
