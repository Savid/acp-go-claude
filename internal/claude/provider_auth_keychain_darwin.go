package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// authKeychainTool is the platform keystore command. It is a package variable
// so the removal ladder is exercised without a real login Keychain.
var authKeychainTool = func(ctx context.Context, args []string) (int, error) {
	command := exec.CommandContext(ctx, "security", args...)

	err := command.Run()
	if err == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}

	return 0, err
}

// RemoveAuthKeychainItems removes both of a config dir's items, across both
// reachable name shapes. Native logout already removes what it knows about;
// this closes the case where the legacy API-key item survives it, which would
// leave a usable credential behind.
func RemoveAuthKeychainItems(ctx context.Context, configDir string, user string) error {
	var failures error

	for _, item := range AuthKeychainItems(configDir, user) {
		callCtx, cancel := context.WithTimeout(ctx, authKeychainCallTimeout)
		code, err := authKeychainTool(callCtx, []string{"delete-generic-password", "-s", item.Service, "-a", item.Account})

		cancel()

		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("remove keychain item: %w", err))

			continue
		}

		if code != 0 && !authKeychainAbsent(code) {
			failures = errors.Join(failures, fmt.Errorf("remove keychain item: status %d", code))
		}
	}

	return failures
}
