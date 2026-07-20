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
)

func copyClaudeResumeCredential(source string, destination string) error {
	data, err := readClaudeResumeCredential(source)
	if err != nil || data == nil {
		return err
	}
	defer clear(data)

	if !validClaudeResumeCredential(data) {
		return errClaudeResumeCredentialMalformed
	}

	return writeClaudeResumeCredential(destination, data)
}

func readClaudeResumeCredential(source string) ([]byte, error) {
	sourcePath := filepath.Join(source, claudeResumeCredentialFile)
	sourceInfo, err := os.Lstat(sourcePath)

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

	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return nil, errClaudeResumeCredentialSource
	}
	defer sourceRoot.Close()

	rootSourceInfo, err := sourceRoot.Lstat(claudeResumeCredentialFile)
	if err != nil || !rootSourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, rootSourceInfo) {
		return nil, errClaudeResumeCredentialUnsafe
	}

	sourceFile, err := sourceRoot.Open(claudeResumeCredentialFile)
	if err != nil {
		return nil, errClaudeResumeCredentialUnreadable
	}

	openedSourceInfo, statErr := sourceFile.Stat()
	if statErr != nil || !openedSourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedSourceInfo) {
		_ = sourceFile.Close()

		return nil, errClaudeResumeCredentialUnsafe
	}

	data, readErr := io.ReadAll(io.LimitReader(sourceFile, claudeResumeCredentialMaxBytes+1))

	closeErr := sourceFile.Close()
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
	destinationInfo, err := os.Lstat(destination)
	if err != nil || !destinationInfo.IsDir() {
		return errClaudeResumeCredentialDestination
	}

	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		return errClaudeResumeCredentialDestination
	}
	defer destinationRoot.Close()

	rootDestinationInfo, err := destinationRoot.Stat(".")
	if err != nil || !os.SameFile(destinationInfo, rootDestinationInfo) {
		return errClaudeResumeCredentialDestination
	}

	chmodErr := destinationRoot.Chmod(".", 0o700)
	if chmodErr != nil {
		return errClaudeResumeCredentialDestination
	}

	return writeClaudeResumeCredentialFile(destinationRoot, data)
}

func writeClaudeResumeCredentialFile(destinationRoot *os.Root, data []byte) error {
	destinationFile, err := destinationRoot.OpenFile(
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
			_ = destinationRoot.Remove(claudeResumeCredentialFile)
		}
	}()

	written, writeErr := destinationFile.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}

	chmodErr := destinationFile.Chmod(0o600)

	closeErr := destinationFile.Close()
	if writeErr != nil || chmodErr != nil || closeErr != nil {
		return errClaudeResumeCredentialDestination
	}

	removeDestination = false

	return nil
}
