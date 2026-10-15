# shellcheck shell=bash
# Shared helpers for nibiru deploy scripts. Source, do not execute.
UID_NUM="$(id -u)"
DOMAIN="gui/${UID_NUM}"
LA_DIR="$HOME/Library/LaunchAgents"
CC_LABEL="com.cc-connect.service"
RUNNER_LABEL="com.nibiru.sakura-runner"
CC_PLIST="$LA_DIR/$CC_LABEL.plist"
RUNNER_PLIST="$LA_DIR/$RUNNER_LABEL.plist"
CC_CONFIG="$HOME/.cc-connect/config.toml"
# Pre-a2a config, written by the operator before merging the a2a project. Rollback restores it.
CC_BASELINE="$CC_CONFIG.runner-baseline"
BIN="$HOME/.local/nibiru/bin/cc-connect"
STAMP="$(date +%Y%m%d%H%M%S)"
DRY=0

parse_dry_run() {
  for a in "$@"; do
    case "$a" in
      --dry-run) DRY=1 ;;
      *) echo "unknown argument: $a (supported: --dry-run)" >&2; exit 2 ;;
    esac
  done
}

# run prints the command; executes it only outside --dry-run.
run() {
  printf '+ %s\n' "$*"
  [ "$DRY" -eq 1 ] && return 0
  "$@"
}

# has_a2a FILE: succeeds if FILE declares an a2a platform (type = "a2a").
has_a2a() {
  grep -Eq "type[[:space:]]*=[[:space:]]*[\"']a2a[\"']" "$1"
}

# service_loaded LABEL: succeeds if launchd still has the service loaded.
service_loaded() {
  launchctl print "$DOMAIN/$1" >/dev/null 2>&1
}

# stop_service LABEL: bootout, then confirm the service is really unloaded.
# Failures are never swallowed: a service that is still loaded is an error,
# because starting its replacement would leave two consumers.
stop_service() {
  local label="$1"
  if [ "$DRY" -eq 1 ]; then
    run launchctl bootout "$DOMAIN/$label"
    echo "+ confirm $label unloaded"
    return 0
  fi
  if ! service_loaded "$label"; then
    echo "  $label not loaded; nothing to stop"
    return 0
  fi
  run launchctl bootout "$DOMAIN/$label" || { echo "bootout $label failed" >&2; return 1; }
  if service_loaded "$label"; then
    echo "refusing: $label is still loaded after bootout" >&2
    return 1
  fi
}
