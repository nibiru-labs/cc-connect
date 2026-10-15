#!/usr/bin/env bash
# Roll back: cc-connect stops consuming the A2A inbox, the python runner takes it over again.
# The newest config backup made by switch-to-a2a.sh is restored (if any), cc-connect is reloaded
# (so the a2a project is gone), and only then is the runner bootstrapped.
# Usage: rollback-to-runner.sh [--dry-run]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
parse_dry_run "$@"

BAK="$(ls -1t "$CC_CONFIG".bak-a2a-* 2>/dev/null | head -1 || true)"
if [ -n "$BAK" ]; then
  run cp -p "$CC_CONFIG" "$CC_CONFIG.bak-pre-rollback-$STAMP"
  run cp -p "$BAK" "$CC_CONFIG"
else
  echo "note: no $CC_CONFIG.bak-a2a-* found; remove the a2a project from the config by hand" >&2
  [ "$DRY" -eq 1 ] || { echo "refusing to roll back without a config backup" >&2; exit 1; }
fi
# 1. stop the a2a consumer (reload cc-connect with the restored config)
run_soft launchctl bootout "$DOMAIN/$CC_LABEL"
run launchctl bootstrap "$DOMAIN" "$CC_PLIST"
# 2. only then start the old consumer
run launchctl bootstrap "$DOMAIN" "$RUNNER_PLIST"
echo "rolled back. verify: tail -f ~/.config/nibiru-a2a/runner.log"
