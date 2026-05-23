# Resume From File

This example imports a Claude JSONL transcript from `session.jsonl` in this
directory, loads the session through ACP so previous interactions are replayed,
sends one no-tools smoke-test prompt, then accepts one typed prompt from stdin.
It denies tool permissions by default so a copied session cannot silently run
commands while you are checking resume behavior.

Use it with a real Claude transcript:

```sh
cd examples/resume-from-file
cp ~/.claude/projects/<project-key>/<session-id>.jsonl ./session.jsonl
go run . -session-id <session-id> -cwd /absolute/path/to/project
```

If the JSONL rows include `session_id` or `sessionId`, `-session-id` can be
omitted. The import step uses the Claude-specific `_claude/session/import`
extension; the replay/resume step uses normal ACP `session/load`, and prompts
use normal ACP `session/prompt`.

Pass `-prompt "..."` or trailing text to change the smoke-test turn. After that
turn completes, type one more message and press enter. Press enter on a blank
line, or press Ctrl-C, to close the session.
