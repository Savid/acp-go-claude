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
	defer file.Close()

	for _, row := range rows {
		if _, err := file.Write(append(append([]byte(nil), row...), '\n')); err != nil {
			return err
		}
	}

	if err := file.Sync(); err != nil {
		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(file.Name(), path)
}
func ValidateRows(rows [][]byte, id string) error {
	found := false

	for _, row := range rows {
		var entry struct {
			SessionID string `json:"sessionId"`
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
