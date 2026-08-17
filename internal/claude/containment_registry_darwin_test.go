//go:build darwin

package claude

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDarwinGenerationRecordCurrentFormatAndDistinctIdentity(t *testing.T) {
	parent := t.TempDir()
	root1 := filepath.Join(parent, "acp-go-claude-runtime-one")
	root2 := filepath.Join(parent, "acp-go-claude-runtime-two")
	for _, root := range []string{root1, root2} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first, err := NewDarwinGenerationRecord(parent, root1, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDarwinGenerationRecord(parent, root2, "discovery")
	if err != nil {
		t.Fatal(err)
	}
	if first.RuntimeID == second.RuntimeID || len(first.RuntimeID) != 32 || len(second.RuntimeID) != 32 {
		t.Fatalf("runtime ids = %q, %q", first.RuntimeID, second.RuntimeID)
	}
	if startErr := first.started(os.Getpid(), syscall.Getpgrp()); startErr != nil {
		t.Fatal(startErr)
	}
	if finishErr := first.finish(true); finishErr != nil {
		t.Fatal(finishErr)
	}

	recordPath := filepath.Join(parent, darwinRegistryDir, first.RuntimeID+".json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{
		"schema_version", "vendor", "containment", "lifecycle_kind", "runtime_id", "generation_root",
		"wrapper_pid", "wrapper_start_sec", "wrapper_start_usec", "direct_child_pid",
		"direct_child_start_sec", "direct_child_start_usec", "original_pgid", "state",
	}
	for _, field := range wantFields {
		if _, ok := fields[field]; !ok {
			t.Fatalf("record missing %q: %s", field, data)
		}
	}
	if fields["state"] != "group_absent" || fields["containment"] != "best_effort" {
		t.Fatalf("record = %s", data)
	}
	if info, err := os.Stat(recordPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(parent, darwinRegistryDir)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("registry mode = %v, %v", info, err)
	}
}

func TestDarwinRegistrySerializesConcurrentReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-concurrent")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}

	_, records, err := readDarwinRecords(parent, generation.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}

	record := records[0]
	registry := filepath.Join(parent, darwinRegistryDir)
	staleTemporary := filepath.Join(registry, "."+record.RuntimeID+"."+strconv.Itoa(os.Getpid())+".tmp")
	if writeErr := os.WriteFile(staleTemporary, []byte("stale"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	const replacements = 64
	errCh := make(chan error, replacements)
	var wait sync.WaitGroup

	for index := range replacements {
		wait.Add(1)

		go func() {
			defer wait.Done()

			copyRecord := record
			if index%2 == 0 {
				copyRecord.State = darwinStateGroupAbsent
			} else {
				copyRecord.State = darwinStateIncomplete
			}

			errCh <- replaceDarwinRecord(parent, copyRecord)
		}()
	}

	wait.Wait()
	close(errCh)

	for replaceErr := range errCh {
		if replaceErr != nil {
			t.Fatal(replaceErr)
		}
	}

	_, records, err = readDarwinRecords(parent, generation.RuntimeID)
	if err != nil || len(records) != 1 {
		t.Fatalf("read replaced record: count=%d err=%v", len(records), err)
	}
}

func TestParseDarwinProcessEnvironmentRejectsMarkerArgvAndAmbiguity(t *testing.T) {
	raw := darwinProcargsFixture([]string{"tool", DarwinRuntimeIDEnv + "=argv-spoof"}, []string{
		DarwinRuntimeIDEnv + "=real-runtime",
		DarwinScratchRootEnv + "=/real/root",
	})
	env, err := parseDarwinProcessEnvironment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env[DarwinRuntimeIDEnv] != "real-runtime" || env[DarwinScratchRootEnv] != "/real/root" {
		t.Fatalf("environment = %#v", env)
	}
	paddedTail := append(append([]byte{}, raw...), 0, 'n', 'o', 'n', 'z', 'e', 'r', 'o')
	env, err = parseDarwinProcessEnvironment(paddedTail)
	if err != nil || env[DarwinRuntimeIDEnv] != "real-runtime" || env[DarwinScratchRootEnv] != "/real/root" {
		t.Fatalf("environment with kernel padding = %#v, %v", env, err)
	}

	if _, err := parseDarwinProcessEnvironment(darwinProcargsFixture(nil, nil)); err == nil {
		t.Fatal("zero argc was accepted")
	}
	if _, err := parseDarwinProcessEnvironment(darwinProcargsFixture([]string{"tool", ""}, nil)); err == nil {
		t.Fatal("empty argv entry was accepted")
	}
	if _, err := parseDarwinProcessEnvironment([]byte{1, 2}); err == nil {
		t.Fatal("truncated procargs was accepted")
	}
	padded := darwinProcargsFixture([]string{"tool"}, []string{DarwinRuntimeIDEnv + "=smuggled"})
	envStart := 4 + len("/bin/tool") + 2 + len("tool") + 1
	padded = append(padded[:envStart], append([]byte{0}, padded[envStart:]...)...)
	if _, err := parseDarwinProcessEnvironment(padded); err == nil {
		t.Fatal("post-argv NUL padding was accepted")
	}
}

func TestDarwinCandidateRevalidationRejectsUIDChange(t *testing.T) {
	identity, err := currentDarwinProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	originalSameUID := darwinIdentitySameUID
	darwinIdentitySameUID = func(darwinProcessIdentity) bool { return false }
	t.Cleanup(func() { darwinIdentitySameUID = originalSameUID })
	candidate := DarwinContainmentCandidate{PID: identity.PID, StartSec: identity.StartSec, StartUsec: identity.StartUsec}
	_, status := revalidateDarwinCandidate(darwinContainmentRecord{}, candidate)
	if status != darwinCandidateAmbiguous {
		t.Fatalf("UID-changed candidate status = %d", status)
	}
}

func TestDarwinDiagnoseIsReadOnly(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-diagnose")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(parent, darwinRegistryDir, generation.RuntimeID+".json")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if diagnoseErr := DiagnoseDarwinContainment(parent, &output); diagnoseErr != nil {
		t.Fatal(diagnoseErr)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !strings.Contains(output.String(), darwinContainmentWarning) {
		t.Fatalf("diagnose mutated record or omitted warning: %q", output.String())
	}
}

func TestDarwinCleanupRequiresSelectionAndSignalsFreshIndividualCandidates(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-cleanup")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupDarwinContainment(parent, generation.RuntimeID, false, &bytes.Buffer{}); err == nil {
		t.Fatal("cleanup accepted missing force")
	}
	if err := CleanupDarwinContainment(parent, "bad", true, &bytes.Buffer{}); err == nil {
		t.Fatal("cleanup accepted invalid runtime id")
	}
	if err := CleanupDarwinContainment(parent, strings.Repeat("f", 32), true, &bytes.Buffer{}); err == nil {
		t.Fatal("cleanup accepted missing selected record")
	}

	originalCandidates := darwinContainmentCandidates
	originalRevalidate := darwinContainmentRevalidate
	originalSignal := darwinContainmentSignalPID
	originalNow := darwinCleanupNow
	originalSleep := darwinCleanupSleep
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinContainmentRevalidate = originalRevalidate
		darwinContainmentSignalPID = originalSignal
		darwinCleanupNow = originalNow
		darwinCleanupSleep = originalSleep
	})

	first := DarwinContainmentCandidate{PID: 101, StartSec: 1, StartUsec: 1}
	spawnedDuringGrace := DarwinContainmentCandidate{PID: 202, StartSec: 2, StartUsec: 2}
	scans := [][]DarwinContainmentCandidate{{first}, {first, spawnedDuringGrace}, {first, spawnedDuringGrace}, {}}
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		result := scans[0]
		scans = scans[1:]

		return result, nil
	}
	darwinContainmentRevalidate = func(_ darwinContainmentRecord, candidate DarwinContainmentCandidate) (DarwinContainmentCandidate, darwinCandidateStatus) {
		return candidate, darwinCandidateCorrelated
	}
	type signalCall struct {
		pid    int
		signal syscall.Signal
	}
	var calls []signalCall
	darwinContainmentSignalPID = func(pid int, signal syscall.Signal) error {
		calls = append(calls, signalCall{pid: pid, signal: signal})

		return nil
	}
	now := time.Unix(1, 0)
	darwinCleanupNow = func() time.Time { return now }
	darwinCleanupSleep = func(duration time.Duration) { now = now.Add(duration) }
	var output bytes.Buffer
	if err := CleanupDarwinContainment(parent, generation.RuntimeID, true, &output); err != nil {
		t.Fatal(err)
	}
	want := []signalCall{{101, syscall.SIGTERM}, {202, syscall.SIGTERM}, {101, syscall.SIGKILL}, {202, syscall.SIGKILL}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("signal calls = %#v, want %#v", calls, want)
	}
	for _, call := range calls {
		if call.pid <= 0 {
			t.Fatalf("cleanup attempted broad/group signal: %#v", calls)
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected root still exists: %v", err)
	}
}

func TestDarwinCleanupDeadlineStartsBeforeRegistryEnumeration(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-deadline")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}

	originalReadDir := darwinRegistryReadDir
	originalCandidates := darwinContainmentCandidates
	originalRevalidate := darwinContainmentRevalidate
	originalSignal := darwinContainmentSignalPID
	originalNow := darwinCleanupNow
	originalSleep := darwinCleanupSleep
	t.Cleanup(func() {
		darwinRegistryReadDir = originalReadDir
		darwinContainmentCandidates = originalCandidates
		darwinContainmentRevalidate = originalRevalidate
		darwinContainmentSignalPID = originalSignal
		darwinCleanupNow = originalNow
		darwinCleanupSleep = originalSleep
	})

	now := time.Unix(100, 0)
	darwinCleanupNow = func() time.Time { return now }
	darwinRegistryReadDir = func(path string) ([]os.DirEntry, error) {
		entries, readErr := originalReadDir(path)
		now = now.Add(defaultCloseWait + time.Second)

		return entries, readErr
	}
	candidate := DarwinContainmentCandidate{PID: 5151, StartSec: 1, StartUsec: 1}
	scans := 0
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		scans++
		if scans == 1 {
			return []DarwinContainmentCandidate{candidate}, nil
		}

		return nil, nil
	}
	darwinContainmentRevalidate = func(_ darwinContainmentRecord, value DarwinContainmentCandidate) (DarwinContainmentCandidate, darwinCandidateStatus) {
		return value, darwinCandidateCorrelated
	}
	signals := 0
	darwinContainmentSignalPID = func(int, syscall.Signal) error {
		signals++

		return nil
	}
	darwinCleanupSleep = func(time.Duration) {}

	var output bytes.Buffer
	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, &output)
	if err == nil || !strings.Contains(err.Error(), "deadline reached during registry enumeration") {
		t.Fatalf("cleanup deadline error = %v", err)
	}
	if signals != 0 || scans != 0 || output.Len() != 0 {
		t.Fatalf("signals=%d scans=%d report=%s", signals, scans, output.String())
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("deadline-expired cleanup removed its generation root: %v", statErr)
	}
}

func TestDarwinCleanupDeadlineIsRecheckedAfterEveryRevalidation(t *testing.T) {
	originalRevalidate := darwinContainmentRevalidate
	originalSignal := darwinContainmentSignalPID
	originalNow := darwinCleanupNow
	t.Cleanup(func() {
		darwinContainmentRevalidate = originalRevalidate
		darwinContainmentSignalPID = originalSignal
		darwinCleanupNow = originalNow
	})

	now := time.Unix(200, 0)
	deadline := now.Add(time.Second)
	darwinCleanupNow = func() time.Time { return now }
	candidate := DarwinContainmentCandidate{PID: 6161, StartSec: 1, StartUsec: 2}
	darwinContainmentRevalidate = func(_ darwinContainmentRecord, value DarwinContainmentCandidate) (DarwinContainmentCandidate, darwinCandidateStatus) {
		now = deadline

		return value, darwinCandidateCorrelated
	}
	darwinContainmentSignalPID = func(int, syscall.Signal) error {
		t.Fatal("cleanup signaled after candidate revalidation exhausted its deadline")

		return nil
	}

	cleanup := darwinCleanupState{
		deadline:       deadline,
		ambiguous:      make(map[int]struct{}),
		termIdentities: make(map[DarwinContainmentCandidate]struct{}),
	}
	signaled, err := cleanup.signalTermCandidates([]DarwinContainmentCandidate{candidate})
	if err != nil || signaled {
		t.Fatalf("post-revalidation TERM = %v, %v", signaled, err)
	}
	if _, ok := cleanup.ambiguous[candidate.PID]; !ok {
		t.Fatal("deadline-expired TERM candidate was not retained as ambiguous")
	}

	now = time.Unix(300, 0)
	deadline = now.Add(time.Second)
	cleanup.deadline = deadline
	cleanup.termIdentities[candidate] = struct{}{}
	if err := cleanup.signalKillCandidates([]DarwinContainmentCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.killSignaled) != 0 {
		t.Fatalf("post-revalidation KILL signals = %v", cleanup.killSignaled)
	}
}

func TestDarwinCleanupEmptyScanBudgetExpiryRetainsRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-empty-deadline")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
	if err != nil {
		t.Fatal(err)
	}

	originalCandidates := darwinContainmentCandidates
	originalSignal := darwinContainmentSignalPID
	originalNow := darwinCleanupNow
	originalRemove := darwinRemoveGenerationRoot
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinContainmentSignalPID = originalSignal
		darwinCleanupNow = originalNow
		darwinRemoveGenerationRoot = originalRemove
	})

	now := time.Unix(400, 0)
	deadline := now.Add(defaultCloseWait)
	darwinCleanupNow = func() time.Time { return now }
	scans := 0
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		scans++
		now = deadline

		return []DarwinContainmentCandidate{}, nil
	}
	darwinContainmentSignalPID = func(int, syscall.Signal) error {
		t.Fatal("deadline-terminal cleanup must not signal")

		return nil
	}
	removals := 0
	darwinRemoveGenerationRoot = func(string) error {
		removals++

		return nil
	}

	var output bytes.Buffer
	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, &output)
	if err == nil || !strings.Contains(err.Error(), "deadline reached before completion") {
		t.Fatalf("cleanup error = %v", err)
	}
	if scans != 1 || removals != 0 || !strings.Contains(output.String(), `"root_removed":false`) {
		t.Fatalf("scans=%d removals=%d report=%s", scans, removals, output.String())
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("deadline-expired cleanup removed its generation root: %v", statErr)
	}
}

func TestDarwinCleanupRootRemovalConsumesBudget(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-removal-deadline")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
	if err != nil {
		t.Fatal(err)
	}

	originalCandidates := darwinContainmentCandidates
	originalNow := darwinCleanupNow
	originalSleep := darwinCleanupSleep
	originalRemove := darwinRemoveGenerationRoot
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinCleanupNow = originalNow
		darwinCleanupSleep = originalSleep
		darwinRemoveGenerationRoot = originalRemove
	})

	now := time.Unix(450, 0)
	deadline := now.Add(defaultCloseWait)
	darwinCleanupNow = func() time.Time { return now }
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		return nil, nil
	}
	darwinCleanupSleep = func(time.Duration) {}
	darwinRemoveGenerationRoot = func(path string) error {
		if removeErr := originalRemove(path); removeErr != nil {
			return removeErr
		}

		now = deadline

		return nil
	}

	var output bytes.Buffer
	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, &output)
	if err == nil || !strings.Contains(err.Error(), "deadline reached before completion") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !strings.Contains(output.String(), `"root_removed":true`) {
		t.Fatalf("cleanup report = %s", output.String())
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("removed generation root still exists: %v", statErr)
	}
}

func TestDarwinCleanupRejectsReplacedRootIdentity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-identity-race")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
	if err != nil {
		t.Fatal(err)
	}

	originalCandidates := darwinContainmentCandidates
	originalSleep := darwinCleanupSleep
	originalRemove := darwinRemoveGenerationRoot
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinCleanupSleep = originalSleep
		darwinRemoveGenerationRoot = originalRemove
	})

	movedRoot := filepath.Join(parent, "original-generation")
	replacementMarker := filepath.Join(root, "replacement-marker")
	scans := 0
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		scans++
		if scans == 1 {
			if renameErr := os.Rename(root, movedRoot); renameErr != nil {
				return nil, renameErr
			}
			if mkdirErr := os.Mkdir(root, 0o700); mkdirErr != nil {
				return nil, mkdirErr
			}
			if writeErr := os.WriteFile(replacementMarker, []byte("unrelated"), 0o600); writeErr != nil {
				return nil, writeErr
			}
		}

		return nil, nil
	}
	darwinCleanupSleep = func(time.Duration) {}
	removals := 0
	darwinRemoveGenerationRoot = func(string) error {
		removals++

		return nil
	}

	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("cleanup error = %v", err)
	}
	if removals != 0 {
		t.Fatalf("recursive removals = %d", removals)
	}
	if _, statErr := os.Stat(replacementMarker); statErr != nil {
		t.Fatalf("replacement root was modified: %v", statErr)
	}
}

func TestDarwinCleanupRepeatsParentContainmentBeforeRemoval(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-parent-race")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
	if err != nil {
		t.Fatal(err)
	}

	originalCandidates := darwinContainmentCandidates
	originalEvalSymlinks := darwinEvalSymlinks
	originalRemove := darwinRemoveGenerationRoot
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinEvalSymlinks = originalEvalSymlinks
		darwinRemoveGenerationRoot = originalRemove
	})

	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		return nil, nil
	}
	parentResolutions := 0
	darwinEvalSymlinks = func(path string) (string, error) {
		if path == parent {
			parentResolutions++
			if parentResolutions == 2 {
				return filepath.Join(parent, "replacement-parent"), nil
			}
		}

		return originalEvalSymlinks(path)
	}
	removals := 0
	darwinRemoveGenerationRoot = func(string) error {
		removals++

		return nil
	}

	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "outside the scratch parent") {
		t.Fatalf("cleanup error = %v", err)
	}
	if parentResolutions != 2 {
		t.Fatalf("scratch parent resolutions = %d", parentResolutions)
	}
	if removals != 0 {
		t.Fatalf("recursive removals = %d", removals)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("generation root was modified: %v", statErr)
	}
}

func TestDarwinCleanupDeadlineCheckpoints(t *testing.T) {
	originalCandidates := darwinContainmentCandidates
	originalRevalidate := darwinContainmentRevalidate
	originalSignal := darwinContainmentSignalPID
	originalNow := darwinCleanupNow
	originalSleep := darwinCleanupSleep
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinContainmentRevalidate = originalRevalidate
		darwinContainmentSignalPID = originalSignal
		darwinCleanupNow = originalNow
		darwinCleanupSleep = originalSleep
	})

	for threshold := 2; threshold <= 48; threshold++ {
		t.Run(strconv.Itoa(threshold), func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "acp-go-claude-runtime-checkpoint")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
			if err != nil {
				t.Fatal(err)
			}

			base := time.Unix(500, 0)
			deadline := base.Add(defaultCloseWait)
			calls := 0
			darwinCleanupNow = func() time.Time {
				calls++
				if calls >= threshold {
					return deadline
				}

				return base
			}
			a := DarwinContainmentCandidate{PID: 7101, StartSec: 1, StartUsec: 1}
			b := DarwinContainmentCandidate{PID: 7102, StartSec: 2, StartUsec: 2}
			scans := 0
			darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
				scans++
				if scans == 1 {
					return []DarwinContainmentCandidate{a}, nil
				}

				return []DarwinContainmentCandidate{b}, nil
			}
			darwinContainmentRevalidate = func(_ darwinContainmentRecord, candidate DarwinContainmentCandidate) (DarwinContainmentCandidate, darwinCandidateStatus) {
				return candidate, darwinCandidateCorrelated
			}
			darwinContainmentSignalPID = func(int, syscall.Signal) error { return nil }
			darwinCleanupSleep = func(time.Duration) {}

			_ = CleanupDarwinContainment(parent, generation.RuntimeID, true, io.Discard)
		})
	}

	for threshold := 6; threshold <= 18; threshold++ {
		t.Run("empty-"+strconv.Itoa(threshold), func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "acp-go-claude-runtime-empty-checkpoint")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			generation, err := NewDarwinGenerationRecord(parent, root, "discovery")
			if err != nil {
				t.Fatal(err)
			}

			base := time.Unix(600, 0)
			deadline := base.Add(defaultCloseWait)
			calls := 0
			darwinCleanupNow = func() time.Time {
				calls++
				if calls >= threshold {
					return deadline
				}

				return base
			}
			darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
				return nil, nil
			}
			darwinCleanupSleep = func(time.Duration) {}

			_ = CleanupDarwinContainment(parent, generation.RuntimeID, true, io.Discard)
		})
	}
}

func TestDarwinCleanupDoesNotReportESRCHAsSignaled(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-esrch")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	originalCandidates := darwinContainmentCandidates
	originalRevalidate := darwinContainmentRevalidate
	originalSignal := darwinContainmentSignalPID
	originalSleep := darwinCleanupSleep
	t.Cleanup(func() {
		darwinContainmentCandidates = originalCandidates
		darwinContainmentRevalidate = originalRevalidate
		darwinContainmentSignalPID = originalSignal
		darwinCleanupSleep = originalSleep
	})
	candidate := DarwinContainmentCandidate{PID: 303, StartSec: 3, StartUsec: 3}
	scans := [][]DarwinContainmentCandidate{{candidate}, {}, {}, {}}
	darwinContainmentCandidates = func(darwinContainmentRecord) ([]DarwinContainmentCandidate, error) {
		result := scans[0]
		scans = scans[1:]

		return result, nil
	}
	darwinContainmentRevalidate = func(_ darwinContainmentRecord, candidate DarwinContainmentCandidate) (DarwinContainmentCandidate, darwinCandidateStatus) {
		return candidate, darwinCandidateCorrelated
	}
	darwinContainmentSignalPID = func(int, syscall.Signal) error { return syscall.ESRCH }
	darwinCleanupSleep = func(time.Duration) {}
	var output bytes.Buffer
	if err := CleanupDarwinContainment(parent, generation.RuntimeID, true, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"term_signaled_pids":[]`) || !strings.Contains(output.String(), `"kill_signaled_pids":[]`) {
		t.Fatalf("cleanup output = %s", output.String())
	}
}

func TestDarwinCleanupRejectsRootOutsideParent(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	_, records, err := readDarwinRecords(parent, generation.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	record := records[0]
	outsideParent := t.TempDir()
	record.GenerationRoot = filepath.Join(outsideParent, "acp-go-claude-runtime-outside")
	if err := os.Mkdir(record.GenerationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceDarwinRecord(parent, record); err != nil {
		t.Fatal(err)
	}
	if err := CleanupDarwinContainment(parent, generation.RuntimeID, true, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid current-format") {
		t.Fatalf("outside-root cleanup error = %v", err)
	}
}

func TestDarwinCleanupRejectsMissingIncompleteRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-missing")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if removeErr := os.Remove(root); removeErr != nil {
		t.Fatal(removeErr)
	}
	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "missing for an incomplete runtime") {
		t.Fatalf("missing-root cleanup error = %v", err)
	}
}

func TestDarwinRecordValidationRejectsMalformedCurrentFormat(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*darwinContainmentRecord)
	}{
		{name: "state", mutate: func(record *darwinContainmentRecord) { record.State = "complete" }},
		{name: "lifecycle", mutate: func(record *darwinContainmentRecord) { record.LifecycleKind = "unknown" }},
		{name: "relative root", mutate: func(record *darwinContainmentRecord) { record.GenerationRoot = "acp-go-claude-runtime-relative" }},
		{name: "partial child", mutate: func(record *darwinContainmentRecord) { pid := 1; record.DirectChildPID = &pid }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "acp-go-claude-runtime-record")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
			if err != nil {
				t.Fatal(err)
			}
			_, records, err := readDarwinRecords(parent, generation.RuntimeID)
			if err != nil {
				t.Fatal(err)
			}
			record := records[0]
			test.mutate(&record)
			if err := replaceDarwinRecord(parent, record); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readDarwinRecords(parent, generation.RuntimeID); err == nil {
				t.Fatal("malformed current-format record was accepted")
			}
		})
	}
}

func TestDarwinRecordReaderRejectsUnsafeFilesystemObjects(t *testing.T) {
	t.Run("record mode", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "acp-go-claude-runtime-mode")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, darwinRegistryDir, generation.RuntimeID+".json")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDarwinRecords(parent, generation.RuntimeID); err == nil {
			t.Fatal("mode-0644 record was accepted")
		}
	})
	t.Run("record symlink", func(t *testing.T) {
		parent := t.TempDir()
		registry := filepath.Join(parent, darwinRegistryDir)
		if err := os.Mkdir(registry, 0o700); err != nil {
			t.Fatal(err)
		}
		runtimeID := strings.Repeat("a", 32)
		target := filepath.Join(parent, "target.json")
		if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(registry, runtimeID+".json")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDarwinRecords(parent, runtimeID); err == nil {
			t.Fatal("symlink record was accepted")
		}
	})
	t.Run("registry mode", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Mkdir(filepath.Join(parent, darwinRegistryDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDarwinRecords(parent, ""); err == nil {
			t.Fatal("mode-0755 registry was accepted")
		}
	})
	t.Run("registry symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(parent, darwinRegistryDir)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDarwinRecords(parent, ""); err == nil {
			t.Fatal("symlink registry was accepted")
		}
	})
	t.Run("lock symlink", func(t *testing.T) {
		parent := t.TempDir()
		registry, err := ensureDarwinRegistry(parent)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(parent, "lock-target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(registry, ".lock")); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(parent, "acp-go-claude-runtime-lock")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := NewDarwinGenerationRecord(parent, root, "prompt"); err == nil {
			t.Fatal("symlink registry lock was accepted")
		}
	})
}

func TestDarwinCleanupReportsAmbiguousIdentityAndRetainsRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-claude-runtime-ambiguous")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	generation, err := NewDarwinGenerationRecord(parent, root, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := currentDarwinProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_, records, err := readDarwinRecords(parent, generation.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	record := records[0]
	record.DirectChildPID = &identity.PID
	record.DirectChildStartSec = &identity.StartSec
	record.DirectChildStartUsec = &identity.StartUsec
	pgid := identity.PID
	record.OriginalPGID = &pgid
	if replaceErr := replaceDarwinRecord(parent, record); replaceErr != nil {
		t.Fatal(replaceErr)
	}
	var output bytes.Buffer
	err = CleanupDarwinContainment(parent, generation.RuntimeID, true, &output)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !strings.Contains(output.String(), `"ambiguous_pids":[`+strconv.Itoa(os.Getpid())) {
		t.Fatalf("cleanup output = %s", output.String())
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("ambiguous root was removed: %v", err)
	}
}

func darwinProcargsFixture(argv, env []string) []byte {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, uint32(len(argv)))
	raw = append(raw, "/bin/tool"...)
	raw = append(raw, 0, 0)
	for _, value := range argv {
		raw = append(raw, value...)
		raw = append(raw, 0)
	}
	for _, value := range env {
		raw = append(raw, value...)
		raw = append(raw, 0)
	}
	if len(env) > 0 {
		raw = append(raw, 0)
	}

	return raw
}
