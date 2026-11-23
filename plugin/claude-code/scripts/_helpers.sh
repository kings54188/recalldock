#!/bin/bash
# Engram — Shared helpers for Claude Code hooks
# WARNING: Do not read from stdin here — scripts source this before reading their hook input.

RECALLDOCK_PORT="${RECALLDOCK_PORT:-7437}"
RECALLDOCK_URL="${RECALLDOCK_URL:-http://127.0.0.1:${RECALLDOCK_PORT}}"

# ─── Project identity ────────────────────────────────────────────────────────
#
# The project key is owned by the RecallDock core (internal/project): a bound
# root answers with its registered ID, a nested .recalldock/config.json answers
# with its declared name, and only an undeclared checkout falls back to a
# derived name.
#
# Adapters must never derive the key themselves. A name guessed from a git
# remote or a directory basename creates a second bucket for the same
# checkout, so sessions, prompts and observations land in a project the rest
# of RecallDock never reads. Ask the core and fail closed when it cannot
# answer: an unresolved project means no memory write, never a wrong one.
resolve_project() {
  local dir="$1"
  [ -n "$dir" ] || return 0
  # Without jq the URL cannot be encoded safely; answer with no project rather
  # than a wrong one.
  command -v jq >/dev/null 2>&1 || return 0

  local encoded
  encoded=$(printf '%s' "$dir" | jq -sRr @uri)
  [ -n "$encoded" ] || return 0

  curl -sf "${RECALLDOCK_URL}/project/current?cwd=${encoded}" --max-time 2 2>/dev/null \
    | jq -r '.project // empty' 2>/dev/null
}

# jq is a hard dependency of every hook — each one parses the host payload with
# it. A missing jq used to fail completely invisibly: hosts discard hook stderr,
# the scripts exit 0 regardless, so it looks like a healthy hook that simply has
# nothing to say. In practice it means no session row, no memory context, and no
# passive capture.
RECALLDOCK_HOOK_LOG="${RECALLDOCK_HOOK_LOG:-${RECALLDOCK_DATA_DIR:-$HOME/.recalldock}/hook-degraded.log}"

# Returns 0 when jq is usable. Otherwise records the failure durably, reports it
# on stderr, and returns 1 so the caller can decide what is still worth doing.
require_jq() {
  command -v jq >/dev/null 2>&1 && return 0

  case "$RECALLDOCK_HOOK_LOG" in
    */*) mkdir -p "${RECALLDOCK_HOOK_LOG%/*}" 2>/dev/null || true ;;
  esac
  printf '%s jq-missing hook=%s pid=%s engram_data_dir=%s\n' \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || printf 'unknown-time')" \
    "${0##*/}" "$$" "${RECALLDOCK_DATA_DIR:-unset}" \
    >> "$RECALLDOCK_HOOK_LOG" 2>/dev/null || true

  printf 'recalldock: jq not found on PATH — %s did nothing, see %s\n' \
    "${0##*/}" "$RECALLDOCK_HOOK_LOG" >&2
  return 1
}
