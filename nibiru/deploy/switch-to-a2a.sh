#!/usr/bin/env bash
# Switch the A2A inbox consumer from the python runner to cc-connect's a2a platform.
# Exactly one consumer may read the inbox: the runner is stopped BEFORE cc-connect is reloaded.
# Prerequisite: ~/.cc-connect/config.toml already contains an a2a project
# (see config.a2a.example.toml). This script never edits it.
# Usage: switch-to-a2a.sh [--dry-run]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
parse_dry_run "$@"

if [ "$DRY" -eq 0 ]; then
  [ -f "$CC_CONFIG" ] || { echo "missing $CC_CONFIG" >&2; exit 1; }
  grep -Eq '^[[:space:]]*type[[:space:]]*=[[:space:]]*"a2a"' "$CC_CONFIG" \
    || { echo "no a2a platform in $CC_CONFIG; merge config.a2a.example.toml first" >&2; exit 1; }
  [ -x "$BIN" ] || { echo "missing binary $BIN; run build.sh first" >&2; exit 1; }
fi

run cp -p "$CC_CONFIG" "$CC_CONFIG.bak-a2a-$STAMP"
run cp -p "$RUNNER_PLIST" "$RUNNER_PLIST.bak-a2a-$STAMP"
# 1. stop the old consumer first
run_soft launchctl bootout "$DOMAIN/$RUNNER_LABEL"
# 2. reload cc-connect so it picks up the a2a platform
run_soft launchctl bootout "$DOMAIN/$CC_LABEL"
run launchctl bootstrap "$DOMAIN" "$CC_PLIST"
echo "switched. verify: tail -f ~/.cc-connect/logs/cc-connect.log ; rollback: rollback-to-runner.sh"
