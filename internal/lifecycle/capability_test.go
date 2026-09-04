package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLifecycleCapabilityStrictScalar(t *testing.T) {
	t.Parallel()

	var decoded Negotiated
	require.NoError(t, json.Unmarshal([]byte(`{"version":1}`), &decoded))
	require.Equal(t, Version, decoded.Version)

	for _, test := range []struct {
		name string
		data string
	}{
		{"missing", `{}`},
		{"missing with fields", `{"updatesOutsidePrompt":true}`},
		{"other integer", `{"version":2}`},
		{"fractional", `{"version":1.0}`},
		{"other fractional", `{"version":1.5}`},
		{"string", `{"version":"1"}`},
		{"null", `{"version":null}`},
		{"boolean", `{"version":true}`},
		{"object", `{"version":{}}`},
		{"array", `{"version":[]}`},
		{"duplicate", `{"version":1,"version":1}`},
		{"unknown", `{"version":1,"unknown":true}`},
		{"trailing", `{"version":1} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var value Negotiated
			require.Error(t, json.Unmarshal([]byte(test.data), &value))
		})
	}
}

func TestLifecycleCapabilityStructuredFieldsAndMalformedInput(t *testing.T) {
	t.Parallel()

	var decoded Negotiated
	require.NoError(t, json.Unmarshal([]byte(`{
		"version":1,
		"updatesOutsidePrompt":true,
		"authoritativeQuiescence":true,
		"quiescenceSource":"process-containment",
		"activityKinds":["task"]
	}`), &decoded))
	require.Equal(t, Negotiated{
		Version:                 Version,
		UpdatesOutsidePrompt:    true,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}, decoded)

	for _, test := range []struct {
		name string
		data string
	}{
		{"empty", ``},
		{"non-object", `[]`},
		{"truncated member", `{"version":1,`},
		{"truncated object", `{"version":1`},
		{"malformed version", `{"version":`},
		{"malformed updates", `{"version":1,"updatesOutsidePrompt":[]}`},
		{"malformed authoritative quiescence", `{"version":1,"authoritativeQuiescence":[]}`},
		{"malformed quiescence source", `{"version":1,"quiescenceSource":[]}`},
		{"malformed activity kinds", `{"version":1,"activityKinds":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var value Negotiated
			require.Error(t, json.Unmarshal([]byte(test.data), &value))
		})
	}
}
