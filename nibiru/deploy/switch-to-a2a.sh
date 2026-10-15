#!/usr/bin/env bash
# Switch the A2A inbox consumer from the python runner to cc-connect's a2a platform.
# Exactly one consumer may read the inbox: the runner is confirmed unloaded BEFORE cc-connect is reloaded.
# Prerequisites (checked, not created here):
#   - ~/.cc-connect/config.toml.runner-baseline exists and contains NO a2a platform. The operator
#     copies the pre-a2a config there before merging the a2a project; rollback restores this file.
#   - ~/.cc-connect/config.toml already contains an a2a project (merged by hand; this script never edits it).
# Usage: switch-to-a2a.sh [--dry-run]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
parse_dry_run "$@"

if [ "$DRY" -eq 0 ]; then
  [ -f "$CC_CONFIG" ] || { echo "missing $CC_CONFIG" >&2; exit 1; }
  has_a2a "$CC_CONFIG" \
    || { echo "no a2a platform in $CC_CONFIG; merge the a2a project first" >&2; exit 1; }
  [ -f "$CC_BASELINE" ] \
    || { echo "missing $CC_BASELINE; copy the pre-a2a config there before merging a2a" >&2; exit 1; }
  if has_a2a "$CC_BASELINE"; then
    echo "$CC_BASELINE already contains an a2a platform; it must be the pre-a2a config" >&2
    exit 1
  fi
  [ -x "$BIN" ] || { echo "missing binary $BIN; run build.sh first" >&2; exit 1; }
fi

# Keep the a2a config and the plist as they are before this switch (forward reference only;
# rollback uses the baseline, never this backup).
run cp -p "$CC_CONFIG" "$CC_CONFIG.bak-a2a-$STAMP"
run cp -p "$RUNNER_PLIST" "$RUNNER_PLIST.bak-a2a-$STAMP"
# 1. stop the old consumer and confirm it is gone; abort otherwise.
stop_service "$RUNNER_LABEL" || { echo "aborted: old consumer still running, cc-connect not touched" >&2; exit 1; }
# 2. reload cc-connect so it picks up the a2a platform.
stop_service "$CC_LABEL" || { echo "aborted: cc-connect still loaded; runner stays stopped" >&2; exit 1; }
run launchctl bootstrap "$DOMAIN" "$CC_PLIST"
echo "switched. verify: tail -f ~/.cc-connect/logs/cc-connect.log ; rollback: rollback-to-runner.sh"
