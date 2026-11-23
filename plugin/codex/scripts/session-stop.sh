#!/bin/bash
# Engram — Stop hook for Codex (synchronous)
#
# Marks the session as ended via the HTTP API.
# Runs synchronously (Codex does not support async: true).
# The HTTP call is fast enough to complete well within the 5s timeout.

RECALLDOCK_PORT="${RECALLDOCK_PORT:-7437}"
RECALLDOCK_URL="http://127.0.0.1:${RECALLDOCK_PORT}"

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

INPUT=$(cat)

# Only the structured memory_candidates field is admitted
printf '%s' "$INPUT" | recalldock candidate-hook > /dev/null 2>&1

# Ending the session is pure jq work. Without it there is nothing left to do, so
# record the failure rather than exiting 0 as though it had succeeded.
require_jq || exit 0

SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')

if [ -z "$SESSION_ID" ]; then
  exit 0
fi

curl -sf "${RECALLDOCK_URL}/sessions/${SESSION_ID}/end" \
  -X POST \
  -H "Content-Type: application/json" \
  -d '{}' \
  --max-time 2 \
  > /dev/null 2>&1

exit 0
