#!/bin/sh
# factory-doctor: preflight for the subscription factory seats.
#
# Every failure the factory has hit in live runs was environment decay, not
# engine logic: a codex update losing its sandbox helpers, PATH resolving an
# ancient node for the verifier, fixtures carrying Windows-form remotes, stale
# worktrees pinning the feature branch, a dead bridge worker. Each check below
# exists because its absence once cost a 20-minute timeout to discover.
#
# Usage:
#   sh scripts/factory-doctor.sh            # environment checks only (fast)
#   AIMEE_DOCTOR_LIVE=1 sh scripts/factory-doctor.sh   # + one trivial call per seat
#
# Seat probes reuse the live-smoke templates: AIMEE_SMOKE_CMD_<SEAT> with
# {PROMPT}/{OUT_FILE} tokens or stdin. Missing template = seat skipped.
# Exit code: number of failed checks.

BRIDGE_ROOT="${AIMEE_BRIDGE_ROOT:-}"
PROBE_TIMEOUT="${AIMEE_DOCTOR_TIMEOUT_S:-90}"
FAILS=0

say() { printf '%s\n' "$*"; }
pass() { say "  ok    $*"; }
fail() { say "  FAIL  $*"; FAILS=$((FAILS + 1)); }

say "factory doctor $(date -u +%Y-%m-%dT%H:%M:%SZ)"

say "[verifier toolchain]"
NODE_V="$(node --version 2>/dev/null)"
case "$NODE_V" in
  v1[89].*|v[2-9][0-9].*) pass "node on PATH is $NODE_V" ;;
  "") fail "node not on PATH (verifier 'node --test' will fail)" ;;
  *) fail "node on PATH is $NODE_V; verifier needs >=18 first in PATH" ;;
esac
git --version >/dev/null 2>&1 && pass "git present" || fail "git missing"

say "[bridge worker]"
if [ -z "$BRIDGE_ROOT" ]; then
  say "  skip  bridge not configured (set AIMEE_BRIDGE_ROOT on bridge hosts)"
elif [ -d "$BRIDGE_ROOT" ]; then
  OUT="$(printf x | timeout 15 node "$BRIDGE_ROOT/bridge-exec.js" doctor-probe 2>&1)"
  case "$OUT" in
    *"not allowlisted"*) pass "bridge worker answering" ;;
    *timeout*) fail "bridge worker not answering (start bridge-worker.mjs on Windows)" ;;
    *) fail "bridge probe unexpected: $OUT" ;;
  esac
else
  say "  skip  no bridge root at $BRIDGE_ROOT"
fi

say "[codex sandbox]"
# A one-command probe proves the whole Codex exec path when configured.
if [ -n "$AIMEE_DOCTOR_CODEX_SANDBOX_CMD" ]; then
  if OUT="$(printf x | timeout 60 sh -c "$AIMEE_DOCTOR_CODEX_SANDBOX_CMD" 2>&1)" && \
     [ "${OUT##*sandbox-ok*}" != "$OUT" ] || printf '%s' "$OUT" | grep -q sandbox-ok; then
    pass "codex sandbox executes commands"
  else
    fail "codex sandbox broken: $(printf '%s' "$OUT" | tail -c 300)"
  fi
else
  say "  skip  set AIMEE_DOCTOR_CODEX_SANDBOX_CMD to probe the Codex exec path"
fi

say "[repo hygiene]"
if [ -n "$AIMEE_SMOKE_REPO" ]; then
  REMOTE="$(git -C "$AIMEE_SMOKE_REPO" config --get remote.origin.url 2>/dev/null)"
  case "$REMOTE" in
    [A-Za-z]:*) fail "origin remote is Windows-form '$REMOTE'; WSL git reads it as an SSH host" ;;
    "") fail "no origin remote in $AIMEE_SMOKE_REPO" ;;
    *) pass "origin remote form ok ($REMOTE)" ;;
  esac
  EXTRA="$(git -C "$AIMEE_SMOKE_REPO" worktree list --porcelain | grep -c '^worktree ')"
  if [ "$EXTRA" -gt 1 ]; then
    fail "$((EXTRA - 1)) stale worktree(s) pinning branches (git worktree list)"
  else
    pass "no stale worktrees"
  fi
else
  say "  skip  AIMEE_SMOKE_REPO unset"
fi

if [ "$AIMEE_DOCTOR_LIVE" = "1" ]; then
  say "[seat probes: one trivial call each, ${PROBE_TIMEOUT}s cap]"
  for SEAT in LUNA SOL SOL_REVIEW FABLE DEEPSEEK ANTIGRAVITY; do
    TEMPLATE="$(printenv "AIMEE_SMOKE_CMD_$SEAT")"
    [ -z "$TEMPLATE" ] && { say "  skip  $SEAT (no template)"; continue; }
    PROMPT="Reply with exactly: SEAT-OK"
    TMP="$(mktemp -d)"
    CMD="$TEMPLATE"
    STDIN_PROMPT=1
    case "$CMD" in *"{PROMPT}"*) STDIN_PROMPT=0 ;; esac
    # {PROMPT} must stay ONE argv element after substitution, exactly as the
    # live harness passes it, so build the command with eval and a quoted value.
    CMD="$(printf '%s' "$CMD" | sed "s|{PROMPT}|\"\$PROMPT\"|; s|{TASK_FILE}|$TMP/task.md|; s|{OUT_FILE}|$TMP/out.md|")"
    printf '%s' "$PROMPT" > "$TMP/task.md"
    START=$(date +%s)
    if [ "$STDIN_PROMPT" = 1 ]; then
      OUT="$(printf '%s' "$PROMPT" | eval timeout "$PROBE_TIMEOUT" "$CMD" 2>&1)"; RC=$?
    else
      OUT="$(eval timeout "$PROBE_TIMEOUT" "$CMD" 2>&1)"; RC=$?
    fi
    SECS=$(( $(date +%s) - START ))
    [ -s "$TMP/out.md" ] && OUT="$(cat "$TMP/out.md")"
    if [ $RC -eq 0 ] && printf '%s' "$OUT" | grep -q "SEAT-OK"; then
      pass "$SEAT answered in ${SECS}s"
    elif [ $RC -eq 124 ]; then
      fail "$SEAT timed out after ${PROBE_TIMEOUT}s"
    else
      fail "$SEAT rc=$RC in ${SECS}s: $(printf '%s' "$OUT" | tail -c 200)"
    fi
    rm -rf "$TMP"
  done
fi

say "---"
if [ "$FAILS" -eq 0 ]; then say "doctor: all checks passed"; else say "doctor: $FAILS check(s) FAILED"; fi
exit "$FAILS"
