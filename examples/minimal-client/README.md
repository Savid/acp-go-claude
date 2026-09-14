# Minimal Client

Launches `acp-go-claude` as a subprocess, creates one session in the current
directory, sends one prompt, and prints the streamed answer. Every permission
request is allowed once.

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP"
```

A local `claude` must be installed and authenticated; the session inherits your
environment and claude's home exactly as running `claude` in this directory would.
