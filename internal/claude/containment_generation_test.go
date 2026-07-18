package claude

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type generationDirEntry struct {
	info fs.FileInfo
	err  error
}

func (entry generationDirEntry) Name() string               { return entry.info.Name() }
func (entry generationDirEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry generationDirEntry) Type() fs.FileMode          { return entry.info.Mode().Type() }
func (entry generationDirEntry) Info() (fs.FileInfo, error) { return entry.info, entry.err }

func TestDarwinGenerationPrepareCommandCopiesWritableInputs(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"model":"sonnet"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	generation := &DarwinGeneration{RuntimeID: strings.Repeat("a", 32), ScratchRoot: root}
	command := exec.Command("claude", "--mcp-config", mcp, "--settings", settings)
	command.Env = []string{
		"PATH=/bin",
		"TMPDIR=/old",
		DarwinRuntimeIDEnv + "=old",
		DarwinScratchRootEnv + "=/old",
		privateAdapterEnvPrefix + "FAKE=secret",
		"malformed",
	}
	if err := generation.prepareCommand(command); err != nil {
		t.Fatal(err)
	}

	wantMCP := filepath.Join(root, "mcp.json")
	wantSettings := filepath.Join(root, "settings.json")
	if command.Args[2] != wantMCP || command.Args[4] != wantSettings {
		t.Fatalf("rewritten args = %v", command.Args)
	}
	for path, want := range map[string]string{wantMCP: `{"mcpServers":{}}`, wantSettings: `{"model":"sonnet"}`} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("copied %s = %q, err=%v", path, contents, err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "tmp")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("temporary root info=%v err=%v", info, err)
	}

	environment := testEnvironmentMap(command.Env)
	if environment["PATH"] != "/bin" || environment["TMPDIR"] != filepath.Join(root, "tmp") ||
		environment[DarwinRuntimeIDEnv] != generation.RuntimeID || environment[DarwinScratchRootEnv] != root {
		t.Fatalf("environment = %#v", environment)
	}
	if _, ok := environment[privateAdapterEnvPrefix+"FAKE"]; ok {
		t.Fatalf("private launch key survived: %#v", environment)
	}
	if _, ok := environment["malformed"]; ok {
		t.Fatalf("malformed environment survived: %#v", environment)
	}
}

func testEnvironmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}

	return values
}

func TestDarwinGenerationPrepareCommandErrors(t *testing.T) {
	if err := (*DarwinGeneration)(nil).prepareCommand(exec.Command("claude")); err == nil {
		t.Fatal("nil generation was accepted")
	}
	if err := (&DarwinGeneration{}).prepareCommand(nil); err == nil {
		t.Fatal("nil command was accepted")
	}

	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&DarwinGeneration{ScratchRoot: fileRoot}).prepareCommand(exec.Command("claude")); err == nil {
		t.Fatal("file scratch root was accepted")
	}

	missing := exec.Command("claude", "--mcp-config", filepath.Join(t.TempDir(), "missing"))
	if err := (&DarwinGeneration{ScratchRoot: t.TempDir()}).prepareCommand(missing); err == nil {
		t.Fatal("missing command input was accepted")
	}

	root := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFailure := exec.Command("claude", "--settings", settings)
	if err := (&DarwinGeneration{ScratchRoot: root}).prepareCommand(writeFailure); err == nil {
		t.Fatal("command input write failure was accepted")
	}
}

func TestDarwinGenerationPathAndEnvironmentHelpers(t *testing.T) {
	parent := t.TempDir()
	if !pathWithin(parent, filepath.Join(parent, "child")) || pathWithin(parent, parent) || pathWithin(parent, filepath.Dir(parent)) {
		t.Fatal("path containment mismatch")
	}

	filtered := withoutPrivateAdapterEnv([]string{
		"A=B",
		privateAdapterEnvPrefix + "BAD=value",
		"acp_go_claude_internal_lower=value",
		"malformed",
	})
	if strings.Join(filtered, ",") != "A=B,malformed" {
		t.Fatalf("filtered environment = %v", filtered)
	}
}

func TestDarwinGenerationLifecycleHelpers(t *testing.T) {
	want := errors.New("failure")
	if err := (*DarwinGeneration)(nil).started(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := (&DarwinGeneration{}).started(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := (&DarwinGeneration{RecordStarted: func(int, int) error { return want }}).started(1, 1); !errors.Is(err, want) {
		t.Fatalf("start error = %v", err)
	}
	if err := (*DarwinGeneration)(nil).finish(true); err != nil {
		t.Fatal(err)
	}

	started := 0
	released := 0
	generation := &DarwinGeneration{
		RecordFinished: func(complete bool) error {
			if !complete {
				t.Fatal("complete = false")
			}
			started++

			return nil
		},
		Release: func(complete bool) error {
			released++

			return want
		},
	}
	if err := generation.finish(true); !errors.Is(err, want) {
		t.Fatalf("release error = %v", err)
	}
	if err := generation.finish(false); !errors.Is(err, want) || started != 1 || released != 1 {
		t.Fatalf("memoized finish error=%v started=%d released=%d", err, started, released)
	}

	released = 0
	generation = &DarwinGeneration{
		RecordFinished: func(bool) error { return want },
		Release: func(bool) error {
			released++

			return nil
		},
	}
	if err := generation.finish(false); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), want.Error()) || released != 0 {
		t.Fatalf("record failure error=%v released=%d", err, released)
	}
}
