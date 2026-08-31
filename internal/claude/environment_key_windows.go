//go:build windows

package claude

import "strings"

// EnvironmentKey returns the case-insensitive Windows variable identity.
func EnvironmentKey(key string) string { return strings.ToUpper(key) }
