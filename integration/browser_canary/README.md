# Real-native browser canary

This fixture drives `claude auth login` through the adapter's production
provider-auth path, then uses passive `execve` tracing to require a real browser
attempt and prove every launcher resolved inside the production-generated shim.
The runtime container has no GUI, credentials, host mounts, or network.

- Claude Code: `2.1.221`, from the [official release](https://github.com/anthropics/claude-code/releases/tag/v2.1.221)
- linux x64 archive SHA-256: `9b6f16520af4f47622fec82b4b2218645b675adaf39438c87625221f07f5e70f`
- linux arm64 archive SHA-256: `2d59431c116aec070516fec3dcf3d4e1447a62665aee899eb74b086a1dc7e3c7`
- Base image: `debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818`

`prepare.sh` downloads and verifies only the native release. Image construction
installs the exact `strace` package, and its context allowlist includes only the
two binaries, entrypoint, and decoy. The final container executes with Docker
`--network none`.
