package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testProcessIsolationConfigPath = "/test/process-isolation.json"

func stubProcessIsolationConfig(t *testing.T) {
	t.Helper()

	original := processIsolationConfigLoader
	processIsolationConfigLoader = func(path string) (processIsolationConfig, error) {
		if path != testProcessIsolationConfigPath {
			t.Fatalf("process isolation config path = %q", path)
		}

		return processIsolationConfig{
			UID:                 20001,
			GID:                 20001,
			BaseEnvironment:     map[string]string{"PATH": "/usr/bin", "HOME": "/var/empty/acp", "USER": "acp", "LOGNAME": "acp"},
			StandaloneOwnerID:   "test-owner",
			StandaloneStateRoot: "/tmp/claude",
		}, nil
	}
	t.Cleanup(func() { processIsolationConfigLoader = original })
}

func isolatedArgs(args ...string) []string {
	return append([]string{"-" + processIsolationConfigFlag, testProcessIsolationConfigPath}, args...)
}

func TestDecodeProcessIsolationConfigStrict(t *testing.T) {
	config, err := decodeProcessIsolationConfig([]byte(`{"uid":20001,"gid":20002,"baseEnvironment":{"PATH":"/usr/bin"},"inheritEnvironment":["AMP_API_KEY"],"standaloneOwnerId":"deployment-1","standaloneStateRoot":"/var/lib/provider"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.UID != 20001 || config.GID != 20002 || config.BaseEnvironment["PATH"] != "/usr/bin" || len(config.InheritEnvironment) != 1 ||
		config.StandaloneOwnerID != "deployment-1" || config.StandaloneStateRoot != "/var/lib/provider" {
		t.Fatalf("decoded config = %#v", config)
	}

	for _, document := range []string{
		`{"uid":1,"gid":2,"baseEnvironment":{},"unknown":true}`,
		`{"uid":1,"gid":2,"baseEnvironment":{}} {}`,
		`{"uid":1,"uid":2,"gid":2,"baseEnvironment":{}}`,
		`{"uid":1,"gid":2,"baseEnvironment":{},"standaloneOwnerId":"a","standaloneOwnerId":"b"}`,
		`{"uid":1,"gid":2,"baseEnvironment":{},"standaloneStateRoot":"/a","standaloneStateRoot":"/b"}`,
		`{"uid":1,"gid":2,"baseEnvironment":{"PATH":"/bin","PATH":"/usr/bin"}}`,
		`{"uid":1,"gid":2,"baseEnvironment":{}} x`,
		`{"uid":1,"gid":2,}`,
		`[1,]`,
		`[{"uid":1,"uid":2}]`,
		``,
	} {
		if _, err := decodeProcessIsolationConfig([]byte(document)); err == nil {
			t.Fatalf("decode unexpectedly accepted %q", document)
		}
	}
	if _, err := decodeProcessIsolationConfig([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestScanJSONValueRejectsUnexpectedDelimiter(t *testing.T) {
	t.Parallel()

	decoder := json.NewDecoder(bytes.NewReader([]byte(`[1]`)))
	for range 2 {
		if _, err := decoder.Token(); err != nil {
			t.Fatal(err)
		}
	}

	err := scanJSONValue(decoder)
	if err == nil || !strings.Contains(err.Error(), "unexpected JSON delimiter") {
		t.Fatalf("scanJSONValue error = %v", err)
	}
}

func TestRunRequiresProcessIsolationConfig(t *testing.T) {
	var stderr strings.Builder
	if code := run(t.Context(), nil, strings.NewReader(""), &strings.Builder{}, &stderr); code != 2 {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-"+processIsolationConfigFlag+" is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunReportsProcessIsolationConfigLoadFailure(t *testing.T) {
	original := processIsolationConfigLoader
	processIsolationConfigLoader = func(string) (processIsolationConfig, error) {
		return processIsolationConfig{}, errors.New("policy unreadable")
	}
	t.Cleanup(func() { processIsolationConfigLoader = original })

	var stderr strings.Builder
	code := run(t.Context(), isolatedArgs(), strings.NewReader(""), &strings.Builder{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "process isolation: policy unreadable") {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunRequiresHomeToMatchStandaloneStateRoot(t *testing.T) {
	stubProcessIsolationConfig(t)

	var stderr strings.Builder
	code := run(t.Context(), isolatedArgs("-home", "/other"), strings.NewReader(""), &strings.Builder{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "must equal standaloneStateRoot") {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
}
