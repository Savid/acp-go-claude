package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	claudeacp "github.com/savid/acp-go-claude"
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

func TestRunWithoutProcessIsolationConfigUsesOrdinaryMode(t *testing.T) {
	originalServe, originalLoader := serve, processIsolationConfigLoader
	t.Cleanup(func() {
		serve = originalServe
		processIsolationConfigLoader = originalLoader
	})

	processIsolationConfigLoader = func(string) (processIsolationConfig, error) {
		t.Fatal("omitted -process-isolation-config must not load a policy")

		return processIsolationConfig{}, nil
	}

	var got claudeacp.Options

	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...claudeacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}

	var stderr strings.Builder
	if code := run(t.Context(), []string{"-home", "/tmp/ordinary-home"}, strings.NewReader(""), &strings.Builder{}, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}

	if got.ProcessIsolation != nil {
		t.Fatalf("ProcessIsolation = %#v, want nil", got.ProcessIsolation)
	}
	if got.Home != "/tmp/ordinary-home" {
		t.Fatalf("Home = %q", got.Home)
	}
}

func TestRunWithExplicitProcessIsolationConfigIsFailClosed(t *testing.T) {
	originalServe, originalLoader := serve, processIsolationConfigLoader
	t.Cleanup(func() {
		serve = originalServe
		processIsolationConfigLoader = originalLoader
	})

	processIsolationConfigLoader = func(string) (processIsolationConfig, error) {
		return processIsolationConfig{}, errors.New("policy unreadable")
	}
	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		t.Fatal("a rejected explicit policy must never reach Serve")

		return nil
	}

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
