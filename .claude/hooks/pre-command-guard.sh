#!/usr/bin/env bash
set -euo pipefail

payload="$(cat)"
echo "$payload" | grep -E '"tool_name"\s*:\s*"Bash"' >/dev/null || exit 0

if echo "$payload" | grep -E 'rm -rf /|mkfs|dd if=.* of=/dev/' >/dev/null; then
  echo '{"decision":"deny","reason":"Blocked dangerous shell command."}'
  exit 0
fi

exit 0
