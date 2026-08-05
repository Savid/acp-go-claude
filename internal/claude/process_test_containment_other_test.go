//go:build !linux && !darwin

package claude

import "testing"

func useDirectTestContainment(*testing.T) {}
