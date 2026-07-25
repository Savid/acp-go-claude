//go:build windows

package claudeacp

// handoffOpenFlags is empty because the handoff form is unreachable on Windows:
// every file:///C:/... spelling yields the path /C:/..., which filepath.IsAbs
// rejects there, so no block survives uri validation to reach this open. It is
// left unreachable rather than half-enabled with flags this platform has no
// equivalent for.
const handoffOpenFlags = 0
