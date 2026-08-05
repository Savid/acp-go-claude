#!/bin/sh
set -eu

# Official release source: https://github.com/anthropics/claude-code/releases/tag/v2.1.221
version=2.1.221
case "$(uname -m)" in
  x86_64)
    archive=claude-linux-x64.tar.gz
    archive_sha=9b6f16520af4f47622fec82b4b2218645b675adaf39438c87625221f07f5e70f
    binary_sha=60db8e88d42c24b5199c92cfd56ec88370c510c3789c6f364af748354f087ada
    ;;
  aarch64|arm64)
    archive=claude-linux-arm64.tar.gz
    archive_sha=2d59431c116aec070516fec3dcf3d4e1447a62665aee899eb74b086a1dc7e3c7
    binary_sha=d3c59d6bcc4adcf4cd85abca3bc13fa1131a34cb32f982bdf030d83a3b11e700
    ;;
  *)
    echo "unsupported browser-canary architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output_dir="$repo_root/.tmp/browser-canary"
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

url="https://github.com/anthropics/claude-code/releases/download/v${version}/${archive}"
curl --fail --location --proto '=https' --retry 3 --silent --show-error --output "$work_dir/$archive" "$url"
printf '%s  %s\n' "$archive_sha" "$work_dir/$archive" | sha256sum -c -
tar -xzf "$work_dir/$archive" -C "$work_dir" claude
printf '%s  %s\n' "$binary_sha" "$work_dir/claude" | sha256sum -c -

mkdir -p "$output_dir"
install -m 0755 "$work_dir/claude" "$output_dir/native"
