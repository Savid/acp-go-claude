package claudeacp

import "os"

func (a *Agent) quotaScratch() (string, error) {
	if a.options.ScratchDir != "" {
		if err := os.MkdirAll(a.options.ScratchDir, 0o700); err != nil {
			return "", err
		}
	}

	return os.MkdirTemp(a.options.ScratchDir, "acp-go-claude-quota-")
}
