package claudeacp

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestRestoreRejectsMalformedNativeRow(t *testing.T) {
	t.Parallel()

	corrupt := map[string]string{
		"malformed_type":    `{"type":7}`,
		"malformed_message": `{"type":"user","message":7}`,
	}

	for name, row := range corrupt {
		for _, replay := range []bool{true, false} {
			route := "resume"
			if replay {
				route = "load"
			}

			t.Run(name+"_"+route, func(t *testing.T) {
				t.Parallel()

				store := acpcore.NewInMemorySessionStore()
				h := newHarness(t, WithSessionStore(store))
				h.initialize()
				cwd := t.TempDir()
				created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
				require.NoError(t, err)
				_, err = h.prompt(created.SessionId, "HELLO", nil)
				require.NoError(t, err)
				_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
				require.NoError(t, err)

				main := acpcore.SessionKey{SessionID: string(created.SessionId)}
				rows, err := loadEntries(h.ctx(), store, main)
				require.NoError(t, err)
				config, err := loadEntries(h.ctx(), store, acpcore.SessionKey{SessionID: main.SessionID, Subpath: "config"})
				require.NoError(t, err)
				require.NoError(t, store.Replace(h.ctx(), main, []acpcore.SessionStoreReplacement{
					{Key: main, Entries: append(rows, acpcore.SessionStoreEntry(row))},
					{Key: acpcore.SessionKey{SessionID: main.SessionID, Subpath: "config"}, Entries: config},
				}))

				if replay {
					_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
				} else {
					_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
				}

				var refused *acp.RequestError
				require.ErrorAs(t, err, &refused, "restore succeeded after silently discarding or forwarding corrupt stored content")
				require.Equal(t, vendor+"_restore_failed", requestErrorData(t, err)["error"])
			})
		}
	}
}

// A newer native file must pass validation before it can replace the mirror.
func TestRestoreRejectsMalformedNativeExtension(t *testing.T) {
	for _, replay := range []bool{true, false} {
		route := "without_replay"
		if replay {
			route = "with_replay"
		}

		t.Run(route, func(t *testing.T) {
			store := acpcore.NewInMemorySessionStore()
			h := newHarness(t, WithSessionStore(store))
			h.initialize()
			cwd := t.TempDir()
			created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
			require.NoError(t, err)
			_, err = h.prompt(created.SessionId, "HELLO", nil)
			require.NoError(t, err)
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
			require.NoError(t, err)
			before, err := store.Load(h.ctx(), string(created.SessionId))
			require.NoError(t, err)
			var record sessionRecord
			require.NoError(t, json.Unmarshal(before["config"][0], &record))
			file, err := os.OpenFile(record.SessionFile, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = file.WriteString(`{"type":7}` + "\n")
			require.NoError(t, err)
			require.NoError(t, file.Close())

			if replay {
				_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
			} else {
				_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
			}

			require.Error(t, err)
			require.Equal(t, vendor+"_restore_failed", requestErrorData(t, err)["error"])
			after, err := store.Load(h.ctx(), string(created.SessionId))
			require.NoError(t, err)
			require.Equal(t, before, after, "malformed native rows must not replace the valid mirror")
		})
	}
}
