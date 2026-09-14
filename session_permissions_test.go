package claudeacp

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestPermissionAnswerControlsNativeDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, expected string
		outcome        acp.RequestPermissionOutcome
	}{
		{"allow", "allow", acp.NewRequestPermissionOutcomeSelected(permissionOptionAllow)},
		{"deny", "deny", acp.NewRequestPermissionOutcomeSelected(permissionOptionDeny)},
		{"cancelled", "deny", acp.NewRequestPermissionOutcomeCancelled()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.rec.answer = func(acp.RequestPermissionRequest) acp.RequestPermissionResponse {
				return acp.RequestPermissionResponse{Outcome: tc.outcome}
			}
			h.initialize(withLifecycle())
			session := h.newSession()
			_, err := h.prompt(session.SessionId, "PERMISSION", promptMeta(1))
			require.NoError(t, err)
			require.Equal(t, tc.expected, agentText(h.rec.snapshot()))
			require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
		})
	}
}

func TestElicitationCapabilityMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, json string
		form, url  bool
	}{
		{"omitted", `{}`, false, false},
		{"empty", `{"elicitation":{}}`, false, false},
		{"form", `{"elicitation":{"form":{}}}`, true, false},
		{"url", `{"elicitation":{"url":{}}}`, false, true},
		{"both", `{"elicitation":{"form":{},"url":{}}}`, true, true},
		{"null", `{"elicitation":{"form":null,"url":null}}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			var calls atomic.Int32
			h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
				calls.Add(1)

				return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"color": "blue"}}}, nil
			}
			h.initialize(withLifecycle(), func(request *acp.InitializeRequest) {
				require.NoError(t, json.Unmarshal([]byte(tc.json), &request.ClientCapabilities))
			})
			session := h.newSession()
			for index, test := range []struct {
				prompt    string
				supported bool
			}{{"ELICIT", tc.form}, {"ELICIT_URL", tc.url}} {
				before := len(h.rec.snapshot())
				_, err := h.prompt(session.SessionId, test.prompt, promptMeta(index+1))
				require.NoError(t, err)
				expected := "cancel"
				if test.supported {
					expected = elicitationAccept
				}
				require.Equal(t, expected, agentText(h.rec.snapshot()[before:]))
			}
			expectedCalls := int32(0)
			if tc.form {
				expectedCalls++
			}
			if tc.url {
				expectedCalls++
			}
			require.Equal(t, expectedCalls, calls.Load())
			require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
		})
	}
}
