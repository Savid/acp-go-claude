Captured from Claude Code 2.1.278 on 2026-09-20.

An installed `claude -p --input-format stream-json --output-format stream-json
--verbose --include-partial-messages --include-hook-events --permission-mode
bypassPermissions --dangerously-skip-permissions --model haiku --session-id
<uuid>` ran in a temporary working directory with the operator's own login.
One user frame asked it to launch a single background subagent (`run_in_background:
true`) whose only job was to reply `ok`, and to answer `done` at once. The
first turn's `result` (`done`) settled that prompt. With no further input,
Claude Code then ran a second turn on its own to report the finished background
task: a `system` `init`, `system` `status`, `stream_event` records, two
`assistant` records, and a second `result`. The fixture is that second turn,
from the frame after the first `result` to the second `result` inclusive.

The live session id is normalised to `fixture-session`; the working directory
is normalised to `/workspace` and home paths to `/home/operator`; payloads,
uuids, and usage values are otherwise unchanged. The test proves adapter
ownership and settlement for those frames, not provider behavior or whether
the background report turn is scheduled on any given run.
