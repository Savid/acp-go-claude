//go:build windows

package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWindowsOrdinaryChildObservesEnvironment(t *testing.T) {
	if os.Args[len(os.Args)-1] == "windows-environment-child" {
		cwd, err := os.Getwd()
		require.NoError(t, err)
		values := map[string]string{"cwd": cwd, "PATH": os.Getenv("PATH"), "PATHEXT": os.Getenv("PATHEXT"), "HOME": os.Getenv("HOME")}
		require.NoError(t, json.NewEncoder(os.Stdout).Encode(values))
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	selected, ignored, cwd, home := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	image, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(selected, "claude-child.exe"), image, 0o700))
	// Resolving from the ambient or base PATH would find this invalid image.
	require.NoError(t, os.WriteFile(filepath.Join(ignored, "claude-child.exe"), []byte("wrong executable"), 0o700))
	t.Setenv("PATH", ignored)
	environment := BuildEnv(Options{
		OrdinaryEnvironment: map[string]string{"Path": ignored, "PATHEXT": ".CMD", "SystemRoot": os.Getenv("SystemRoot")},
		Env:                 map[string]string{"PATH": selected, "PathExt": ".EXE", "HOME": home},
	})
	process, err := startOrdinaryNative("claude-child", []string{"-test.run=^TestWindowsOrdinaryChildObservesEnvironment$", "--", "windows-environment-child"}, environment, cwd)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = process.Stdin().Close()
		defer process.Stdout().Close()
		defer process.Stderr().Close()
		revokeErr := process.Revoke(cleanup)
		_, waitErr := process.Wait(cleanup)
		require.NoError(t, revokeErr)
		require.NoError(t, waitErr)
	})
	require.NoError(t, process.Stdin().Close())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	require.NoError(t, err)
	require.Zero(t, result.ExitCode)
	observed := map[string]string{}
	require.NoError(t, json.NewDecoder(process.Stdout()).Decode(&observed))
	require.Equal(t, map[string]string{"cwd": cwd, "PATH": selected, "PATHEXT": ".EXE", "HOME": home}, observed)
}
