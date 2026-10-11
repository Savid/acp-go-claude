package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func SessionPath(home, cwd, id string) string {
	return filepath.Join(home, "projects", ProjectDirName(cwd), id+".jsonl")
}

// TranscriptStarted reports whether the transcript at path holds any
// record, reading only up to its first non-blank byte. A missing file holds
// none.
func TranscriptStarted(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	defer func() { _ = file.Close() }()

	buffer := make([]byte, 4<<10)

	for {
		read, readErr := file.Read(buffer)
		if len(bytes.TrimSpace(buffer[:read])) > 0 {
			return true, nil
		}

		if errors.Is(readErr, io.EOF) {
			return false, nil
		}

		if readErr != nil {
			return false, readErr
		}
	}
}

// ValidateRows refuses stored rows that are not this session's transcript:
// a row whose members disagree with the native shape, or a set that names no
// session identity at all. Both restore routes run it before the rows reach
// claude's own home. Every member the replay decoder reads is declared here,
// so decoding the row is what rejects a wrong type; only SessionID is read
// afterwards.
func ValidateRows(rows [][]byte, id string) error {
	found := false

	for _, row := range rows {
		var entry struct {
			SessionID       string   `json:"sessionId"`
			Type            string   `json:"type"`
			UUID            string   `json:"uuid"`
			ParentToolUseID string   `json:"parentToolUseID"` //nolint:tagliatelle // Native transcript spelling.
			Message         *Message `json:"message"`
		}
		if err := json.Unmarshal(row, &entry); err != nil {
			return err
		}

		if entry.SessionID != "" {
			if entry.SessionID != id {
				return fmt.Errorf("transcript session id mismatch")
			}

			found = true
		}
	}

	if !found {
		return errors.New("transcript has no session identity")
	}

	return nil
}
