#!/bin/bash
# Engram — SubagentStop hook for Claude Code (async)
#
# Thin hook: reads the subagent output from stdin, POSTs it to
# the passive capture endpoint. All extraction logic lives in the
# Go server — this script is intentionally minimal.

RECALLDOCK_PORT="${RECALLDOCK_PORT:-7437}"
RECALLDOCK_URL="http://127.0.0.1:${RECALLDOCK_PORT}"

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

# Without jq the subagent output cannot be read out of the payload, which would
# look identical to a subagent that produced nothing.
require_jq || exit 0

# Read hook input from stdin
INPUT=$(cat)
SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')
OUTPUT=$(echo "$INPUT" | jq -r '.stdout // empty')

# Nothing to capture if no output
[ -z "$OUTPUT" ] && exit 0

# Project identity comes from the RecallDock core. Capturing under a guessed
# project would file the observation where the rest of RecallDock never reads
# it, so an unresolved project means no capture.
PROJECT=$(resolve_project "$CWD")
[ -n "$PROJECT" ] || exit 0

# Fire and forget — server handles extraction, dedup, and storage
curl -sf "${RECALLDOCK_URL}/observations/passive" \
  -X POST \
  -H "Content-Type: application/json" \
  -d "$(jq -n \
    --arg sid "$SESSION_ID" \
    --arg content "$OUTPUT" \
    --arg project "$PROJECT" \
    --arg source "subagent-stop" \
    '{session_id: $sid, content: $content, project: $project, source: $source}')" \
  > /dev/null 2>&1

exit 0
