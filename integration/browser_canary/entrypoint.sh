#!/bin/sh
set -eu

case "$(uname -m)" in
  x86_64) native_sha=60db8e88d42c24b5199c92cfd56ec88370c510c3789c6f364af748354f087ada ;;
  aarch64) native_sha=d3c59d6bcc4adcf4cd85abca3bc13fa1131a34cb32f982bdf030d83a3b11e700 ;;
  *) echo "unsupported browser-canary architecture: $(uname -m)" >&2; exit 1 ;;
esac

test -x /usr/local/bin/claude
printf '%s  %s\n' "$native_sha" /usr/local/bin/claude | sha256sum --check --strict
test "$(/usr/local/bin/claude --version)" = "2.1.221 (Claude Code)"

if [ "${1:-}" = "--verify-native" ]; then
  exit 0
fi

rm -f /canary/evidence/browser-escape /canary/evidence/exec.log /canary/evidence/test.log /canary/evidence/launchers
set +e
ACP_GO_CLAUDE_BROWSER_CANARY=1 \
timeout --signal=TERM --kill-after=15 120 \
  strace -f -qq -e trace=execve,execveat -o /canary/evidence/exec.log \
  /canary/browser-canary.test -test.run '^TestRealNativeBrowserContainment$' -test.v \
  >/canary/evidence/test.log 2>&1
status=$?
set -e
cat /canary/evidence/test.log
test "$status" -eq 0
test "$(grep -c '^--- PASS: TestRealNativeBrowserContainment' /canary/evidence/test.log || true)" -eq 1
! grep -q 'testing: warning: no tests to run' /canary/evidence/test.log
grep -q 'execve("/usr/local/bin/claude"' /canary/evidence/exec.log
! grep -Eq 'execveat\([^,]+, "", .*AT_EMPTY_PATH' /canary/evidence/exec.log

sed -n \
  -e 's/.*execve("\([^"]*\)".*/\1/p' \
  -e 's/.*execveat([^,]*, "\([^"]*\)".*/\1/p' \
  /canary/evidence/exec.log \
  | grep -E '/(open|xdg-open|x-www-browser|www-browser|sensible-browser|gio|firefox|google-chrome|google-chrome-stable|chromium|chromium-browser)$' \
  >/canary/evidence/launchers || true
test -s /canary/evidence/launchers
while IFS= read -r launcher; do
  case "$launcher" in
    /canary/scratch/acp-go-claude-browser-shim-*/open|\
    /canary/scratch/acp-go-claude-browser-shim-*/xdg-open|\
    /canary/scratch/acp-go-claude-browser-shim-*/x-www-browser|\
    /canary/scratch/acp-go-claude-browser-shim-*/www-browser|\
    /canary/scratch/acp-go-claude-browser-shim-*/sensible-browser) ;;
    *) echo "browser launcher escaped production shim: $launcher" >&2; exit 1 ;;
  esac
done </canary/evidence/launchers
test ! -e /canary/evidence/browser-escape
