package main

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildVersion(t *testing.T) {
	originalReadBuildInfo := readBuildInfo
	t.Cleanup(func() { readBuildInfo = originalReadBuildInfo })

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true
	}
	require.Equal(t, "v1.2.3", buildVersion())

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: buildInfoVCSRevisionKey, Value: "1234567890abcdef"},
				{Key: buildInfoVCSModifiedKey, Value: "true"},
			},
		}, true
	}
	require.Equal(t, "1234567890ab-dirty", buildVersion())

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{{Key: buildInfoVCSRevisionKey, Value: "abc123"}},
		}, true
	}
	require.Equal(t, "abc123", buildVersion())

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{}, true
	}
	require.Equal(t, developmentVersion, buildVersion())

	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	require.Equal(t, developmentVersion, buildVersion())
}
