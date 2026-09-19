package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func SessionPath(home, cwd, id string) string {
	return filepath.Join(home, "projects", ProjectDirName(cwd), id+".jsonl")
}

func ReadRows(path string) ([][]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer file.Close()

	var rows [][]byte

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)

	for scanner.Scan() {
		row := append([]byte(nil), scanner.Bytes()...)
		if !json.Valid(row) {
			return nil, errors.New("invalid native transcript row")
		}

		rows = append(rows, row)
	}

	return rows, scanner.Err()
}

func WriteRows(path string, rows [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	file, err := os.CreateTemp(filepath.Dir(path), ".transcript-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())

	for _, row := range rows {
		if _, err := file.Write(append(append([]byte(nil), row...), '\n')); err != nil {
			_ = file.Close()

			return err
		}
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(file.Name(), path)
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
