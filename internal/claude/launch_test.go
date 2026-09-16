package claude

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// valueFlags are the launch flags that carry a value, with the value each is
// expected to carry for the fully configured launch below.
var valueFlags = map[string]string{
	"--model":           "haiku",
	"--permission-mode": "plan",
	"--system-prompt":   "be brief",
	"--settings":        "/tmp/settings.json",
	"--json-schema":     `{"type":"object"}`,
}

// flagValue returns the value following name, and whether name is present.
func flagValue(args []string, name string) (string, bool) {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1], true
		}
	}

	return "", false
}

func TestLaunchArgsSelectTheContinuationFlag(t *testing.T) {
	t.Parallel()

	fresh := Launch{SessionID: "session-uuid"}.Args()
	id, ok := flagValue(fresh, "--session-id")
	require.True(t, ok, "a new conversation names its own uuid")
	require.Equal(t, "session-uuid", id)
	require.NotContains(t, fresh, "--resume")

	resumed := Launch{SessionID: "session-uuid", Resume: true}.Args()
	id, ok = flagValue(resumed, "--resume")
	require.True(t, ok, "a conversation with history is continued, not created")
	require.Equal(t, "session-uuid", id)
	require.NotContains(t, resumed, "--session-id")

	for _, args := range [][]string{fresh, resumed} {
		require.Equal(t, []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json"}, args[:5])
		require.Contains(t, args, "--verbose")
		require.Contains(t, args, "--include-partial-messages")
		require.Contains(t, args, "--include-hook-events")
		require.Contains(t, args, "--permission-prompt-tool")
	}
}

func TestLaunchArgsCarryEveryConfiguredMember(t *testing.T) {
	t.Parallel()

	args := Launch{
		SessionID:             "session-uuid",
		Model:                 "haiku",
		PermissionMode:        "plan",
		SystemPrompt:          "be brief",
		Bare:                  true,
		OutputSchema:          map[string]any{"type": "object"},
		SettingSources:        []string{"user", "project"},
		SettingsFile:          "/tmp/settings.json",
		AdditionalDirectories: []string{"/one", "/two"},
	}.Args()

	for name, want := range valueFlags {
		value, ok := flagValue(args, name)
		require.True(t, ok, name)
		require.Equal(t, want, value, name)
	}

	require.Contains(t, args, "--bare")
	require.Contains(t, args, "--setting-sources=user,project")

	directories := make([]string, 0, 2)
	for index, arg := range args {
		if arg == "--add-dir" && index+1 < len(args) {
			directories = append(directories, args[index+1])
		}
	}

	require.Equal(t, []string{"/one", "/two"}, directories)
}

func TestLaunchArgsOmitUnsetMembers(t *testing.T) {
	t.Parallel()

	args := strings.Join(Launch{SessionID: "session-uuid"}.Args(), " ")
	for flag := range valueFlags {
		require.NotContains(t, args, flag)
	}

	for _, flag := range []string{"--bare", "--setting-sources", "--add-dir"} {
		require.NotContains(t, args, flag)
	}

	// An empty source list selects no source at all, which is not the same as
	// leaving native defaults in place.
	require.Contains(t, Launch{SessionID: "s", SettingSources: []string{}}.Args(), "--setting-sources=")
}
