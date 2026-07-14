#!/usr/bin/env bash
# a2a-inbox-hook.sh — UserPromptSubmit hook for Claude Code.
# Drains the per-PID inbox file written by `a2abridge bridge` and emits
# a JSON object with `hookSpecificOutput.additionalContext` so the inbox
# is injected into the next turn's context.
#
# Installation: a2abridge install --apply  drops this at
# ~/.claude/hooks/a2a-inbox-hook.sh and registers it in settings.json.

set -euo pipefail

# A hook must never break the user's prompt: if python3 is unavailable
# (minimal containers, stripped distros) skip silently instead of
# crashing under set -e.
command -v python3 >/dev/null 2>&1 || exit 0

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$PWD}"

# Find the LIVE inbox file. The bridge writes inbox-<ppid>.json (ppid = the
# bridge's parent = this claude). On a claude relaunch the OLD inbox-<oldpid>.json
# lingers; if it still holds leftover records, the previous first-non-empty glob
# fixated on the STALE file and never surfaced genuine inbound sitting in the
# live-PID file (the "wake fires but the pending message isn't rendered" bug).
# Fix: skip + prune inbox files whose <pid> is no longer a running process, and
# pick a LIVE-pid non-empty inbox.
#   $CWD/.a2a/inbox-<ppid>.json    (preferred — per project)
#   ~/.a2abridge/state/<ppid>/inbox-<ppid>.json   (fallback)
INBOX=""
for d in "$PROJECT_DIR/.a2a" "$HOME/.a2abridge/state"/*; do
  [ -d "$d" ] || continue
  for f in "$d"/inbox-*.json; do
    [ -e "$f" ] || continue
    pid="${f##*/inbox-}"; pid="${pid%.json}"
    # Prune a stale-PID inbox (its owning bridge process is gone) — left in place
    # it shadows the live inbox and genuine inbound never surfaces on the wake.
    if [ -n "$pid" ] && [ "$pid" -eq "$pid" ] 2>/dev/null && ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$f" 2>/dev/null
      continue
    fi
    [ -s "$f" ] || continue
    INBOX="$f"
    break 2
  done
done

if [ -z "$INBOX" ]; then
  exit 0
fi

# Render inbox to Markdown via python3 (always available in macOS / WSL2 / Linux).
RENDERED=$(python3 - "$INBOX" <<'PY' 2>/dev/null
import json, sys
try:
    with open(sys.argv[1]) as f:
        msgs = json.load(f)
except Exception:
    sys.exit(0)
if not msgs:
    sys.exit(0)
out = ["You have %d unread A2A message(s):" % len(msgs), ""]
for m in msgs:
    sender = m.get("from") or "unknown"
    task_id = m.get("taskId", "")
    text = (m.get("text") or "").replace("\n", " ").strip()
    out.append(f"- **from `{sender}`** (task `{task_id}`): {text}")
out.append("")
out.append("Process each message *before* answering the user. Reply with `a2a_complete_task`, then continue with the user's prompt.")
print("\n".join(out))
PY
)

[ -z "$RENDERED" ] && exit 0

python3 - "$RENDERED" <<'PY'
import json, sys
print(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit",
        "additionalContext": sys.argv[1]
    }
}))
PY

# Drain after successful render.
: > "$INBOX"
