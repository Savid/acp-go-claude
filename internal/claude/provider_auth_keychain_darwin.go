package claude

import (
	"bytes"
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

// authKeychainFlagService and authKeychainFlagAccount are the platform
// tool's item-selector flags, shared by the read and removal legs.
const (
	authKeychainFlagService = "-s"
	authKeychainFlagAccount = "-a"
)

// authKeychainReadTool is the platform keystore read command. It exists apart
// from authKeychainTool because a read's answer is the secret itself, so
// stdout must be captured. A package variable so the read leg is exercised
// without a real login Keychain.
var authKeychainReadTool = func(ctx context.Context, args []string) ([]byte, int, error) {
	command := exec.CommandContext(ctx, "security", args...)

	output, err := command.Output()
	if err == nil {
		return output, 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, exitErr.ExitCode(), nil
	}

	return nil, 0, err
}

// ReadAuthKeychainCredential answers with the composite OAuth credential blob
// the login Keychain holds for a config dir, or nil when no item exists. A
// native login stores the blob only in the Keychain, keyed by a hash of the
// config dir path, so this read is the only way credential material leaves an
// instance home whose plaintext store was never written. The platform tool
// appends one newline the blob never carries; only that newline is trimmed.
func ReadAuthKeychainCredential(ctx context.Context, configDir string, user string) ([]byte, error) {
	var failures error

	for _, item := range AuthKeychainCredentialItems(configDir, user) {
		callCtx, cancel := context.WithTimeout(ctx, authKeychainCallTimeout)
		output, code, err := authKeychainReadTool(callCtx, []string{
			"find-generic-password",
			authKeychainFlagService, item.Service,
			authKeychainFlagAccount, item.Account,
			"-w",
		})

		cancel()

		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("read keychain item: %w", err))

			continue
		}

		if code == 0 {
			// An item holding nothing is not a credential; reading it as one
			// would materialize an empty blob a resumed session then fails on.
			if value := bytes.TrimSuffix(output, []byte("\n")); len(bytes.TrimSpace(value)) > 0 {
				return value, nil
			}

			continue
		}

		if !authKeychainAbsent(code) {
			failures = errors.Join(failures, fmt.Errorf("read keychain item: status %d", code))
		}
	}

	return nil, failures
}

// RemoveAuthKeychainItems removes both of a config dir's items, across both
// reachable name shapes. Native logout already removes what it knows about;
// this closes the case where the legacy API-key item survives it, which would
// leave a usable credential behind.
func RemoveAuthKeychainItems(ctx context.Context, configDir string, user string) error {
	var failures error

	for _, item := range AuthKeychainItems(configDir, user) {
		callCtx, cancel := context.WithTimeout(ctx, authKeychainCallTimeout)
		code, err := authKeychainTool(callCtx, []string{
			"delete-generic-password",
			authKeychainFlagService, item.Service,
			authKeychainFlagAccount, item.Account,
		})

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
