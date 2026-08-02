package claudeacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	claudeResumeCredentialFile     = ".credentials.json"
	claudeResumeCredentialMaxBytes = 1 << 20
)

var (
	errClaudeResumeCredentialSource      = errors.New("claude resume credential source is unavailable")
	errClaudeResumeCredentialUnsafe      = errors.New("claude resume credential source is unsafe")
	errClaudeResumeCredentialOversized   = errors.New("claude resume credential exceeds size limit")
	errClaudeResumeCredentialUnreadable  = errors.New("claude resume credential source is unreadable")
	errClaudeResumeCredentialMalformed   = errors.New("claude resume credential source is malformed")
	errClaudeResumeCredentialDestination = errors.New("claude resume credential destination is unavailable")

	resumeCredentialLstat        = os.Lstat
	resumeCredentialOpenRoot     = os.OpenRoot
	resumeCredentialRootLstat    = func(root *os.Root, name string) (os.FileInfo, error) { return root.Lstat(name) }
	resumeCredentialRootOpen     = func(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
	resumeCredentialFileStat     = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
	resumeCredentialReadAll      = io.ReadAll
	resumeCredentialFileClose    = func(file *os.File) error { return file.Close() }
	resumeCredentialRootStat     = func(root *os.Root, name string) (os.FileInfo, error) { return root.Stat(name) }
	resumeCredentialRootChmod    = func(root *os.Root, name string, mode os.FileMode) error { return root.Chmod(name, mode) }
	resumeCredentialRootOpenFile = func(root *os.Root, name string, flag int, mode os.FileMode) (*os.File, error) {
		return root.OpenFile(name, flag, mode)
	}
	resumeCredentialRootRemove = func(root *os.Root, name string) error { return root.Remove(name) }
	resumeCredentialFileWrite  = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	resumeCredentialFileChmod  = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }

	// resumeCredentialKeystore is the platform keystore leg of the resume
	// copy. On darwin it reads the login Keychain item a native login stores
	// for the source config dir; elsewhere it answers absence. A package
	// variable so unit tests exercise the precedence without a real keystore.
	resumeCredentialKeystore = readClaudeResumeKeychainCredential
)

func copyClaudeResumeCredential(source string, destination string) error {
	// The keystore is consulted before the plaintext file because the CLI
	// itself prefers its Keychain item when both exist: a config dir can hold
	// a stale credential file beside a live Keychain item, and carrying the
	// file would resume the session logged out.
	data, err := resumeCredentialKeystore(source)
	if err != nil {
		return err
	}

	if data == nil {
		data, err = readClaudeResumeCredential(source)
		if err != nil || data == nil {
			return err
		}
	}
	defer clear(data)

	if !validClaudeResumeCredential(data) {
		return errClaudeResumeCredentialMalformed
	}

	return writeClaudeResumeCredential(destination, data)
}

func readClaudeResumeCredential(source string) ([]byte, error) {
	sourcePath := filepath.Join(source, claudeResumeCredentialFile)
	sourceInfo, err := resumeCredentialLstat(sourcePath)

	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, errClaudeResumeCredentialSource
	}

	if !sourceInfo.Mode().IsRegular() {
		return nil, errClaudeResumeCredentialUnsafe
	}

	if sourceInfo.Size() < 0 || sourceInfo.Size() > claudeResumeCredentialMaxBytes {
		return nil, errClaudeResumeCredentialOversized
	}

	sourceRoot, err := resumeCredentialOpenRoot(source)
	if err != nil {
		return nil, errClaudeResumeCredentialSource
	}
	defer sourceRoot.Close()

	rootSourceInfo, err := resumeCredentialRootLstat(sourceRoot, claudeResumeCredentialFile)
	if err != nil || !rootSourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, rootSourceInfo) {
		return nil, errClaudeResumeCredentialUnsafe
	}

	sourceFile, err := resumeCredentialRootOpen(sourceRoot, claudeResumeCredentialFile)
	if err != nil {
		return nil, errClaudeResumeCredentialUnreadable
	}

	openedSourceInfo, statErr := resumeCredentialFileStat(sourceFile)
	if statErr != nil || !openedSourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedSourceInfo) {
		_ = resumeCredentialFileClose(sourceFile)

		return nil, errClaudeResumeCredentialUnsafe
	}

	data, readErr := resumeCredentialReadAll(io.LimitReader(sourceFile, claudeResumeCredentialMaxBytes+1))

	closeErr := resumeCredentialFileClose(sourceFile)
	if readErr != nil || closeErr != nil {
		clear(data)

		return nil, errClaudeResumeCredentialUnreadable
	}

	if len(data) > claudeResumeCredentialMaxBytes {
		clear(data)

		return nil, errClaudeResumeCredentialOversized
	}

	return data, nil
}

func validClaudeResumeCredential(data []byte) bool {
	trimmed := bytes.TrimSpace(data)

	return json.Valid(data) && len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func writeClaudeResumeCredential(destination string, data []byte) error {
	destinationInfo, err := resumeCredentialLstat(destination)
	if err != nil || !destinationInfo.IsDir() {
		return errClaudeResumeCredentialDestination
	}

	destinationRoot, err := resumeCredentialOpenRoot(destination)
	if err != nil {
		return errClaudeResumeCredentialDestination
	}
	defer destinationRoot.Close()

	rootDestinationInfo, err := resumeCredentialRootStat(destinationRoot, ".")
	if err != nil || !os.SameFile(destinationInfo, rootDestinationInfo) {
		return errClaudeResumeCredentialDestination
	}

	chmodErr := resumeCredentialRootChmod(destinationRoot, ".", 0o700)
	if chmodErr != nil {
		return errClaudeResumeCredentialDestination
	}

	return writeClaudeResumeCredentialFile(destinationRoot, data)
}

func writeClaudeResumeCredentialFile(destinationRoot *os.Root, data []byte) error {
	destinationFile, err := resumeCredentialRootOpenFile(
		destinationRoot,
		claudeResumeCredentialFile,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return errClaudeResumeCredentialDestination
	}

	removeDestination := true
	defer func() {
		if removeDestination {
			_ = resumeCredentialRootRemove(destinationRoot, claudeResumeCredentialFile)
		}
	}()

	written, writeErr := resumeCredentialFileWrite(destinationFile, data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}

	chmodErr := resumeCredentialFileChmod(destinationFile, 0o600)

	closeErr := resumeCredentialFileClose(destinationFile)
	if writeErr != nil || chmodErr != nil || closeErr != nil {
		return errClaudeResumeCredentialDestination
	}

	removeDestination = false

	return nil
}
