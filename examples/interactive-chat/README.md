# Interactive Chat

This example is an interactive ACP client that launches `acp-go-claude` as a
subprocess, initializes the connection, and creates a session, then runs a
read-eval-print loop that sends each line you type as a prompt and streams the
assistant messages, thoughts, tool calls, and usage back to the terminal.

```sh
go run ./examples/interactive-chat
```

At the `claude> ` prompt, Enter submits the current line, Esc interrupts the
running turn, and Ctrl-C exits. Type `/exit` or `/quit` to leave. An optional
trailing argument is sent as the first prompt before the loop begins. A local
`claude` CLI must be installed and authenticated.
