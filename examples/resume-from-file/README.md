# Resume From File

This example reads a Claude JSONL transcript from `session.jsonl` in this
directory into a `SessionStore`, loads the session through ACP so previous
interactions are replayed, then sends one no-tools smoke-test prompt in-process.
It denies tool permissions by default so a copied session cannot silently run
commands while you are checking resume behavior.

Use it with a real Claude transcript:

```sh
cd examples/resume-from-file
cp ~/.claude/projects/<project-key>/<session-id>.jsonl ./session.jsonl
go run . -session <session-id> -cwd /absolute/path/to/project
```

If the JSONL rows include a `sessionId` (or `session_id`), `-session` can be
omitted and the id is inferred; `-cwd` likewise defaults to the transcript cwd
or the current directory. Loading uses normal ACP `session/load`, and the prompt
uses normal ACP `session/prompt`.

Pass `-prompt "..."` to change the smoke-test turn, `-path` to point at a
specific `claude` CLI, and `-home` to set the parent root for isolated Claude session state.
