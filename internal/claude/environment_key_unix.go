//go:build !windows

package claude

// EnvironmentKey returns the case-sensitive Unix variable identity.
func EnvironmentKey(key string) string { return key }
