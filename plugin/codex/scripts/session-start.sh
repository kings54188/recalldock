#!/bin/bash
# Engram — SessionStart hook for Codex
#
# 1. Ensures the recalldock server is running
# 2. Creates a session in recalldock
# 3. Auto-imports git-synced chunks if .recalldock/manifest.json exists
# 4. Injects Memory Protocol instructions + memory context

RECALLDOCK_PORT="${RECALLDOCK_PORT:-7437}"
RECALLDOCK_URL="http://127.0.0.1:${RECALLDOCK_PORT}"
IMPORT_TIMEOUT_SECS=8
LOCK_TTL_SECS=$((IMPORT_TIMEOUT_SECS + 4))
LOCK_METADATA_STALE_SECS=$((LOCK_TTL_SECS * 5))

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

# A missing jq cannot be fatal here: the server still has to be started and the
# protocol still has to be handed over. But the session row and the memory
# context are both jq work, so announce it in the context the model is about to
# receive rather than degrading in silence.
if ! require_jq; then
  printf 'recalldock: memory hooks are DEGRADED — jq is missing from PATH, so this session was not registered and no memory context was injected. See %s\n\n' "$RECALLDOCK_HOOK_LOG"
fi

# Read hook input from stdin
INPUT=$(cat)
exec 0<&-   # close hook stdin so no child can keep the host waiting on it
SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

# Ensure recalldock server is running. Detached on purpose: a hook must not leave a
# background job or an inherited stdin behind, and a second server on the same
# port is harmless because the health check runs first.
if ! curl -sf "${RECALLDOCK_URL}/health" --max-time 1 > /dev/null 2>&1; then
  nohup "${RECALLDOCK_BIN:-recalldock}" serve </dev/null >/dev/null 2>&1 &
  disown 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    curl -sf "${RECALLDOCK_URL}/health" --max-time 1 > /dev/null 2>&1 && break
    sleep 0.3
  done
fi

# Project identity comes from the RecallDock core, never from a name guessed
# from the git remote or the directory. An unresolved project means no session
# row and no context — never a session filed under the wrong project.
PROJECT=$(resolve_project "$CWD")

# Create session
if [ -n "$SESSION_ID" ] && [ -n "$PROJECT" ]; then
  curl -sf "${RECALLDOCK_URL}/sessions" \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg id "$SESSION_ID" --arg project "$PROJECT" --arg dir "$CWD" \
      '{id: $id, project: $project, directory: $dir}')" \
    > /dev/null 2>&1
fi

# Auto-import git-synced chunks
if [ -f "${CWD}/.recalldock/manifest.json" ]; then
  (
    cd "$CWD" 2>/dev/null || exit 0
    IMPORT_LOCK="/tmp/recalldock-sync-import-$(printf '%s' "$CWD" | cksum | cut -d ' ' -f 1).lock"
    write_import_lock_info() {
      LOCK_INFO_TMP="$IMPORT_LOCK/info.$$"
      printf '%s %s\n' "$LOCK_PID" "$LOCK_NOW" > "$LOCK_INFO_TMP" 2>/dev/null \
        && mv "$LOCK_INFO_TMP" "$IMPORT_LOCK/info" 2>/dev/null
    }
    lock_path_age_secs() {
      LOCK_PATH=$1
      LOCK_MTIME=""
      if LOCK_MTIME=$(stat -f %m "$LOCK_PATH" 2>/dev/null); then
        :
      elif LOCK_MTIME=$(stat -c %Y "$LOCK_PATH" 2>/dev/null); then
        :
      else
        return 1
      fi
      case "$LOCK_MTIME" in
        ''|*[!0-9]*) return 1 ;;
      esac
      printf '%s\n' $(( LOCK_NOW - LOCK_MTIME ))
    }
    lock_metadata_is_stale() {
      LOCK_METADATA_PATH=$1
      LOCK_METADATA_AGE=$(lock_path_age_secs "$LOCK_METADATA_PATH") || return 1
      [ "$LOCK_METADATA_AGE" -gt "$LOCK_METADATA_STALE_SECS" ]
    }
    acquire_import_lock() {
      LOCK_PID="${BASHPID:-$$}"
      LOCK_NOW=$(date +%s)
      if mkdir "$IMPORT_LOCK" 2>/dev/null; then
        write_import_lock_info || true
        return 0
      fi

      LOCK_INFO=""
      if [ -f "$IMPORT_LOCK/info" ]; then
        LOCK_INFO=$(cat "$IMPORT_LOCK/info" 2>/dev/null || true)
      fi
      read -r OLD_PID OLD_EPOCH LOCK_INFO_EXTRA <<< "$LOCK_INFO"
      STALE_LOCK=0
      if [ -z "$LOCK_INFO" ] || [ -z "$OLD_PID" ] || [ -z "$OLD_EPOCH" ] || [ -n "$LOCK_INFO_EXTRA" ]; then
        lock_metadata_is_stale "$IMPORT_LOCK" || return 1
        STALE_LOCK=1
      elif [[ "$OLD_PID" == *[!0-9]* ]] || [[ "$OLD_EPOCH" == *[!0-9]* ]]; then
        lock_metadata_is_stale "$IMPORT_LOCK/info" || return 1
        STALE_LOCK=1
      elif kill -0 "$OLD_PID" 2>/dev/null; then
        return 1
      elif [ $(( LOCK_NOW - OLD_EPOCH )) -gt "$LOCK_TTL_SECS" ]; then
        STALE_LOCK=1
      fi

      if [ "$STALE_LOCK" -ne 1 ]; then
        return 1
      fi
      rm -f "$IMPORT_LOCK/info" 2>/dev/null || true
      rmdir "$IMPORT_LOCK" 2>/dev/null || return 1
      if mkdir "$IMPORT_LOCK" 2>/dev/null; then
        write_import_lock_info || true
        return 0
      fi
      return 1
    }
    if ! acquire_import_lock; then
      exit 0
    fi
    trap 'rm -f "$IMPORT_LOCK/info" 2>/dev/null || true; rmdir "$IMPORT_LOCK" 2>/dev/null || true' EXIT
    if command -v timeout >/dev/null 2>&1; then
      timeout "${IMPORT_TIMEOUT_SECS}s" recalldock sync --import >/dev/null 2>&1 || true
    else
      recalldock sync --import >/dev/null 2>&1 &
      IMPORT_PID=$!
      (sleep "$IMPORT_TIMEOUT_SECS"; kill "$IMPORT_PID" 2>/dev/null || true) &
      WAITER_PID=$!
      wait "$IMPORT_PID" 2>/dev/null || true
      kill "$WAITER_PID" 2>/dev/null || true
    fi
  ) >/dev/null 2>&1 &
fi

# Fetch context only after an exact project selection. An empty query would
# let the server fall back to its own cwd and return another project's memory.
CONTEXT=""
if [ -n "$PROJECT" ]; then
  ENCODED_PROJECT=$(printf '%s' "$PROJECT" | jq -sRr @uri)
  CONTEXT=$(curl -sf "${RECALLDOCK_URL}/context?project=${ENCODED_PROJECT}" --max-time 3 2>/dev/null | jq -r '.context // empty')
fi

# Inject Memory Protocol + context — stdout is returned to Codex as additionalContext
cat <<'PROTOCOL'
## Engram Persistent Memory — ACTIVE PROTOCOL

You have recalldock memory tools. This protocol is MANDATORY and ALWAYS ACTIVE.

### CORE TOOLS — always available, no ToolSearch needed
mem_save, mem_search, mem_context, mem_session_summary, mem_get_observation, mem_save_prompt

Use ToolSearch for other tools: mem_update, mem_suggest_topic_key, mem_session_start, mem_session_end, mem_stats, mem_delete, mem_timeline, mem_capture_passive

### PROACTIVE SAVE — do NOT wait for user to ask
Call `mem_save` IMMEDIATELY after ANY of these:
- Decision made (architecture, convention, workflow, tool choice)
- Bug fixed (include root cause)
- Convention or workflow documented/updated
- Notion/Jira/GitHub artifact created or updated with significant content
- Non-obvious discovery, gotcha, or edge case found
- Pattern established (naming, structure, approach)
- User preference or constraint learned
- Feature implemented with non-obvious approach
- User confirms your recommendation ("go with that", "sounds good", or the equivalent in the user's language)
- User rejects an approach or expresses a preference ("no, better X", "I prefer X", or the equivalent in the user's language)
- Discussion concludes with a clear direction chosen

**Self-check after EVERY task**: "Did I or the user just make a decision, confirm a recommendation, express a preference, fix a bug, learn something, or establish a convention? If yes → mem_save NOW."

### SEARCH MEMORY when:
- User asks to recall anything ("remember", "what did we do", or the equivalent in the user's language)
- Starting work on something that might have been done before
- User mentions a topic you have no context on
- User's FIRST message references the project, a feature, or a problem — call `mem_search` with keywords from their message to check for prior work before responding

### SESSION CLOSE — before saying "done":
Call `mem_session_summary` with: Goal, Discoveries, Accomplished, Next Steps, Relevant Files.
PROTOCOL

# Inject memory context if available
if [ -n "$CONTEXT" ]; then
  printf "\n%s\n" "$CONTEXT"
fi

exit 0
