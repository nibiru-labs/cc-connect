#!/usr/bin/env bash
# Roll back: cc-connect stops consuming the A2A inbox, the python runner takes it over again.
# Order: confirm cc-connect unloaded -> restore the pre-a2a baseline config (verified to contain no
# a2a platform) -> bootstrap cc-connect with it -> only then bootstrap the runner.
# Usage: rollback-to-runner.sh [--dry-run]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
parse_dry_run "$@"

if [ "$DRY" -eq 0 ]; then
  [ -f "$CC_BASELINE" ] \
    || { echo "missing $CC_BASELINE; refusing to roll back without the pre-a2a config" >&2; exit 1; }
  if has_a2a "$CC_BASELINE"; then
    echo "$CC_BASELINE contains an a2a platform; refusing (it must be the pre-a2a config)" >&2
    exit 1
  fi
  if service_loaded "$RUNNER_LABEL"; then
    echo "refusing: $RUNNER_LABEL is already loaded; a second consumer would start" >&2
    exit 1
  fi
fi
# 1. stop the a2a consumer and confirm it is gone before anything else starts.
stop_service "$CC_LABEL" || { echo "aborted: cc-connect still loaded; runner not started" >&2; exit 1; }
# 2. restore the baseline (the a2a config is kept as a backup).
run cp -p "$CC_CONFIG" "$CC_CONFIG.bak-pre-rollback-$STAMP"
run cp -p "$CC_BASELINE" "$CC_CONFIG"
if [ "$DRY" -eq 0 ] && has_a2a "$CC_CONFIG"; then
  echo "restored config still has a2a; aborting before start" >&2
  exit 1
fi
# 3. start cc-connect with the restored config, then the old consumer.
run launchctl bootstrap "$DOMAIN" "$CC_PLIST"
run launchctl bootstrap "$DOMAIN" "$RUNNER_PLIST"
echo "rolled back. verify: tail -f ~/.config/nibiru-a2a/runner.log"
