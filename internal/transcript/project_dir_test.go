package transcript

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectDirName(t *testing.T) {
	t.Parallel()

	// The long-path cases pin the truncate-and-hash rule Claude Code 2.1.219
	// applies on disk, including both signs of the 32-bit hash it renders as an
	// unsigned base-36 magnitude. A resume transcript written under any other
	// name is invisible to `claude --resume`.
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
			path: "/srv/agent-workspaces/deeply/nested/tenant-alpha/project-whiteboard/live-runs/run-000042/data/workspaces/sessions/ses_00000000-0000-4000-8000-000000000002/_runs/ses_00000000-0000-4000-8000-000000000002",
			want: "-srv-agent-workspaces-deeply-nested-tenant-alpha-project-whiteboard-live-runs-run-000042-data-workspaces-sessions-ses-00000000-0000-4000-8000-000000000002--runs-ses-00000000-0000-4000-8000-00000000000-ef0grw",
		},
		{
			name: "negative hash suffix",
			path: "/srv/agent-workspaces/deeply/nested/tenant-alpha/project-whiteboard/live-runs/run-000042/data/workspaces/sessions/ses_00000000-0000-4000-8000-000000000001/_runs/ses_00000000-0000-4000-8000-000000000001",
			want: "-srv-agent-workspaces-deeply-nested-tenant-alpha-project-whiteboard-live-runs-run-000042-data-workspaces-sessions-ses-00000000-0000-4000-8000-000000000001--runs-ses-00000000-0000-4000-8000-00000000000-1m90qc",
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
