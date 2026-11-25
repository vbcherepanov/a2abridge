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

# Find a non-empty inbox file. The bridge writes a single stable
#   $CWD/.a2a/inbox.json                    (preferred — per project)
#   ~/.a2abridge/state/<ppid>/inbox.json    (fallback)
# Legacy inbox-<pid>.json files left by older bridges are still read during an
# upgrade, but one whose owning process is gone is pruned: left in place it
# shadows the live inbox and genuine inbound never surfaces on the wake.
INBOX=""
for d in "$PROJECT_DIR/.a2a" "$HOME/.a2abridge/state"/*; do
  [ -d "$d" ] || continue
  for f in "$d"/inbox*.json; do
    [ -e "$f" ] || continue
    case "${f##*/}" in
      inbox-*.json)
        pid="${f##*/inbox-}"; pid="${pid%.json}"
        if [ -n "$pid" ] && [ "$pid" -eq "$pid" ] 2>/dev/null && ! kill -0 "$pid" 2>/dev/null; then
          rm -f "$f" 2>/dev/null
          continue
        fi
        ;;
    esac
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
def one_line(s):
    # splitlines also splits on \r, \x85,   and the rest, so nothing a
    # peer sends can start a line of its own.
    return " ".join(str(s or "").splitlines()).strip()

def label(s):
    # A label sits inside backticks: a backtick in it would close the code
    # span and let a peer write its own "(claimed) · token" marker.
    return one_line(s).replace("`", "'")

out = ["You have %d unread A2A message(s):" % len(msgs), ""]
for m in msgs:
    sender = label(m.get("from")) or "unknown"
    task_id = label(m.get("taskId"))
    text = one_line(m.get("text"))
    # authName is verified by the bridge (bearer token); from is what the
    # sender claims about itself.
    auth = label(m.get("authName"))
    who = f"`{sender}` (claimed) · token `{auth}`" if auth else f"`{sender}`"
    out.append(f"- **from {who}** (task `{task_id}`): {text}")
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
