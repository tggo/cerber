#!/usr/bin/env bash
# Stop hook: remind to update DEFINITION_OF_DONE.md.
# If the latest commit (HEAD) changed code (internal/ static/ cmd/ pkg/) but did
# NOT touch DEFINITION_OF_DONE.md, surface a one-time non-blocking reminder.
# A marker in .git keeps it to once per offending commit (no loops, not noisy).
# Any error / nothing to flag -> exit 0 silently.
set -u

root="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
cd "$root" 2>/dev/null || exit 0

head="$(git rev-parse HEAD 2>/dev/null)" || exit 0
marker="$(git rev-parse --git-dir 2>/dev/null)/.dod_reminded" || exit 0

# Already handled this commit? stay silent.
[ -f "$marker" ] && [ "$(cat "$marker" 2>/dev/null)" = "$head" ] && exit 0

files="$(git show --name-only --pretty=format: "$head" 2>/dev/null)" || exit 0

# Record that we've inspected this commit, so we fire at most once for it.
printf '%s' "$head" > "$marker" 2>/dev/null || true

code_changed=0
dod_changed=0
printf '%s\n' "$files" | grep -qE '^(internal|cmd|web)/' && code_changed=1
printf '%s\n' "$files" | grep -qx 'DEFINITION_OF_DONE.md' && dod_changed=1

if [ "$code_changed" = "1" ] && [ "$dod_changed" = "0" ]; then
  short="$(printf '%s' "$head" | cut -c1-9)"
  printf '{"systemMessage":"⚠️ DoD: commit %s changed code but DEFINITION_OF_DONE.md was not updated. Add/update the feature entry (or confirm no observable behaviour change)."}\n' "$short"
fi
exit 0
