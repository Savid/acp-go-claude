//go:build windows

package mapper

// FileURIHostPath converts the path component of a file URI into a host path.
// A Windows file URI carries the drive behind an empty authority, so its path
// component begins with a separator the drive letter must not keep: file:///C:/a
// names C:\a, while "/C:/a" is a rooted path with no volume that filepath.IsAbs
// rejects and filepath.Clean would leave rooted on whatever drive the process
// happens to sit on.
func FileURIHostPath(path string) string {
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' && isDriveLetter(path[1]) {
		return path[1:]
	}

	return path
}

func isDriveLetter(char byte) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}
