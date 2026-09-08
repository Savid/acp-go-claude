package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestColdSessionConfigurationReplacement(t *testing.T) {
	t.Parallel()

	oldPaths := []string{absTestPath("tools", "first"), absTestPath("tools", "second")}
	newPaths := []string{absTestPath("tools", "new")}
	for _, operation := range []string{"load", "resume"} {
		for _, tc := range []struct {
			name    string
			options map[string]any
			token   string
			paths   []string
			writes  int32
		}{
			{name: "env", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}}, token: "rotated", paths: oldPaths, writes: 1},
			{name: "paths", options: map[string]any{"extraPathDirs": newPaths}, token: "stored", paths: newPaths, writes: 1},
			{name: "reordered paths", options: map[string]any{"extraPathDirs": []string{oldPaths[1], oldPaths[0]}}, token: "stored", paths: []string{oldPaths[1], oldPaths[0]}, writes: 1},
			{name: "both", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}, "extraPathDirs": newPaths}, token: "rotated", paths: newPaths, writes: 1},
			{name: "clear env", options: map[string]any{"env": map[string]string{}}, paths: oldPaths, writes: 1},
			{name: "clear paths", options: map[string]any{"extraPathDirs": []string{}}, token: "stored", writes: 1},
			{name: "omitted", token: "stored", paths: oldPaths},
			{name: "matching", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "stored"}, "extraPathDirs": oldPaths}, token: "stored", paths: oldPaths},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				const sessionID acp.SessionId = "33333333-3333-4333-8333-333333333333"
				cwd := t.TempDir()
				store := &countingConfigurationStore{SessionStore: NewInMemorySessionStore()}
				main := SessionKey{SessionID: string(sessionID)}
				sub := SessionKey{SessionID: string(sessionID), Subpath: "subagents/worker"}
				transcript := SessionStoreEntry(`{"type":"user","message":{"content":"remember the original conversation"}}`)
				subEntries := []SessionStoreEntry{[]byte(`{"type":"assistant","message":{"content":"worker history"}}`)}
				require.NoError(t, store.Append(t.Context(), main, testStoredSessionEntries(t,
					ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "stored"}, ExtraPathDirs: oldPaths}, transcript)))
				require.NoError(t, store.Append(t.Context(), sub, subEntries))

				wantedEnv := map[string]string{}
				if tc.token != "" {
					wantedEnv["TOOL_TOKEN"] = tc.token
				}
				for _, options := range []map[string]any{
					tc.options,
					{"env": wantedEnv, "extraPathDirs": slices.Clone(tc.paths)},
					nil,
				} {
					agent, launches := newColdConfigurationNativeAgent(t, store)
					err := coldConfigurationOperation(t.Context(), agent, operation, sessionID, cwd, options)
					require.NoError(t, err)
					request := <-launches
					require.Contains(t, request.Arguments, "--resume")
					require.Contains(t, request.Arguments, string(sessionID))
					environment := map[string]string{}
					for _, entry := range request.Environment {
						key, value, _ := strings.Cut(entry, "=")
						environment[key] = value
					}
					require.Equal(t, tc.token, environment["TOOL_TOKEN"])
					require.Equal(t, strings.Join(append(slices.Clone(tc.paths), "/usr/bin:/bin"), string(os.PathListSeparator)), environment["PATH"])
					require.NoError(t, agent.Close())
					require.Equal(t, tc.writes, store.replacements.Load(), "matching and omitted cold recovery must not replace")
					entries, err := store.Load(t.Context(), main)
					require.NoError(t, err)
					require.Len(t, entries, 2)
					configuration, err := unmarshalSessionConfiguration(entries[0])
					require.NoError(t, err)
					require.True(t, maps.Equal(wantedEnv, configuration.Env))
					require.Equal(t, tc.paths, configuration.ExtraPathDirs)
					require.Equal(t, transcript, entries[1])
					preserved, err := store.Load(t.Context(), sub)
					require.NoError(t, err)
					require.Equal(t, subEntries, preserved)
				}
			})
		}
	}
}

func TestColdConfigurationFailureDoesNotPublishOrPartiallyWrite(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"load", "resume"} {
		for _, failure := range []string{"startup", "replace", "publication"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				t.Parallel()
				const sessionID acp.SessionId = "33333333-3333-4333-8333-333333333333"
				main := SessionKey{SessionID: string(sessionID)}
				sub := SessionKey{SessionID: string(sessionID), Subpath: "subagents/worker"}
				store := &faultSessionStore{SessionStore: NewInMemorySessionStore()}
				original := testStoredSessionEntries(t, ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "stored"}}, []byte(`{"type":"user"}`))
				subEntries := []SessionStoreEntry{[]byte(`{"type":"assistant"}`)}
				require.NoError(t, store.Append(t.Context(), main, original))
				require.NoError(t, store.Append(t.Context(), sub, subEntries))
				transport := newFakeClaudeTransport()
				fault := errors.New("injected " + failure + " failure")
				switch failure {
				case "startup":
					transport.startErr = fault
				case "replace":
					store.replaceErr = fault
				}
				agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store),
					WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
				if failure == "publication" {
					_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
					require.NoError(t, err)
				}
				installFakeClaudeClient(agent, transport)
				t.Cleanup(func() { require.NoError(t, agent.Close()) })
				err := coldConfigurationOperation(t.Context(), agent, operation, sessionID, t.TempDir(),
					map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}})
				if failure == "publication" {
					require.ErrorContains(t, err, `"limit":"active_sessions"`)
				} else {
					require.ErrorIs(t, err, fault)
				}
				require.NotContains(t, agent.sessions, sessionID)
				entries, err := store.Load(t.Context(), main)
				require.NoError(t, err)
				if failure == "publication" {
					require.Len(t, entries, 2)
					configuration, decodeErr := unmarshalSessionConfiguration(entries[0])
					require.NoError(t, decodeErr)
					require.Equal(t, map[string]string{"TOOL_TOKEN": "rotated"}, configuration.Env)
					require.Equal(t, original[1:], entries[1:])
				} else {
					require.Equal(t, original, entries)
				}
				preserved, err := store.Load(t.Context(), sub)
				require.NoError(t, err)
				require.Equal(t, subEntries, preserved)
				if failure != "startup" {
					require.Equal(t, 1, transport.CloseCalls(), "unpublished successor must be contained")
				}
			})
		}
	}
}

type countingConfigurationStore struct {
	SessionStore
	replacements atomic.Int32
}

func (s *countingConfigurationStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	s.replacements.Add(1)

	return s.SessionStore.Replace(ctx, main, replacements)
}

func coldConfigurationOperation(ctx context.Context, agent *Agent, operation string, id acp.SessionId, cwd string, options map[string]any) error {
	meta := WithSessionMeta(map[string]any{"claude": map[string]any{"options": options}})
	if operation == "load" {
		_, err := agent.LoadSession(ctx, LoadSessionRequest(id, cwd, meta))

		return err
	}
	_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, cwd, meta))

	return err
}

// Use the production client and process transport so assertions observe the
// complete environment delivered to the host's native launch boundary.
func newColdConfigurationNativeAgent(t *testing.T, store SessionStore) (*Agent, <-chan NativeRequest) {
	t.Helper()

	launches := make(chan NativeRequest, 1)
	authority := newFakeHostAuthority()
	authority.start = func(_ context.Context, request NativeRequest) (NativeProcess, error) {
		if request.Executable == keychainToolExecutable {
			return valueNativeProcess{}, nil
		}
		if slices.Equal(request.Arguments, []string{"--version"}) {
			return &fakeNativeProcess{authority: authority, stdout: io.NopCloser(strings.NewReader("2.1.263\n"))}, nil
		}
		launches <- request

		return newConfigurationNativeProcess(), nil
	}
	agent := NewAgent(WithHome(t.TempDir()), WithScratchDir(t.TempDir()), WithExecutablePath("fixture-claude"),
		WithHostAuthority(authority), WithSessionStore(store), WithLogger(slog.New(slog.DiscardHandler)))
	agent.setConnection(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	return agent, launches
}

type configurationNativeProcess struct {
	input  *io.PipeReader
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	output *io.PipeWriter
	done   chan struct{}
}

func newConfigurationNativeProcess() *configurationNativeProcess {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	process := &configurationNativeProcess{input: input, stdin: stdin, stdout: stdout, output: output, done: make(chan struct{})}
	go func() {
		defer close(process.done)
		defer output.Close()
		defer input.Close()
		fixture := newFakeClaudeTransport()
		decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
		for {
			var request claude.ControlRequest
			if err := decoder.Decode(&request); err != nil {
				return
			}
			fixture.respond(request)
			if err := encoder.Encode(<-fixture.messages); err != nil {
				return
			}
		}
	}()

	return process
}

func (p *configurationNativeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *configurationNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (*configurationNativeProcess) Stderr() io.ReadCloser   { return io.NopCloser(strings.NewReader("")) }

func (p *configurationNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-p.done:
		return NativeResult{}, nil
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (p *configurationNativeProcess) Revoke(context.Context) error {
	return errors.Join(p.input.Close(), p.output.Close())
}
