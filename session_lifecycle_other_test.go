//go:build !linux

package claudeacp

import "errors"

func startDetachedContainmentChild(string, string, string, string) error {
	return errors.New("detached containment fixture requires Linux")
}
