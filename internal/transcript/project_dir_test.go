package transcript

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectDirName(t *testing.T) {
	t.Parallel()

	// The long-path cases pin directory names Claude Code 2.1.219 actually
	// created on disk. A resume transcript written under any other name is
	// invisible to `claude --resume`.
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "empty",
			path: "",
			want: "-",
		},
		{
			name: "short path is sanitized verbatim",
			path: "/tmp/project_1",
			want: "-tmp-project-1",
		},
		{
			name: "exactly at the limit is not truncated",
			path: "/" + strings.Repeat("a", 199),
			want: "-" + strings.Repeat("a", 199),
		},
		{
			name: "one over the limit is truncated and hashed",
			path: "/" + strings.Repeat("a", 200),
			want: "-" + strings.Repeat("a", 199) + "-b6ymvl",
		},
		{
			name: "positive hash suffix",
			path: "/home/savid/ai/wagie/tests/acceptance/live/runs/20260726T042506Z-3770974/data/workspaces/whiteboard-agent-sessions/ses_93b95d08-a3d1-482d-8f64-20ad84f9672a/_runs/ses_93b95d08-a3d1-482d-8f64-20ad84f9672a",
			want: "-home-savid-ai-wagie-tests-acceptance-live-runs-20260726T042506Z-3770974-data-workspaces-whiteboard-agent-sessions-ses-93b95d08-a3d1-482d-8f64-20ad84f9672a--runs-ses-93b95d08-a3d1-482d-8f64-20ad84f967-tg7t6a",
		},
		{
			name: "negative hash suffix",
			path: "/home/savid/ai/wagie/tests/acceptance/live/runs/20260726T042506Z-3770974/data/workspaces/whiteboard-agent-sessions/ses_9e71ad2e-e1f7-4682-9583-3631f4ebb423/_runs/ses_9e71ad2e-e1f7-4682-9583-3631f4ebb423",
			want: "-home-savid-ai-wagie-tests-acceptance-live-runs-20260726T042506Z-3770974-data-workspaces-whiteboard-agent-sessions-ses-9e71ad2e-e1f7-4682-9583-3631f4ebb423--runs-ses-9e71ad2e-e1f7-4682-9583-3631f4ebb4-mekuq6",
		},
		{
			name: "length and hash count UTF-16 code units",
			path: "/" + strings.Repeat("a", 198) + "\U0001F600",
			want: "-" + strings.Repeat("a", 198) + "--b5wpam",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.want, ProjectDirName(test.path))
		})
	}
}
