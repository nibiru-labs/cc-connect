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

# run_soft is run, but a failing command (e.g. bootout of a service that is not loaded) is not fatal.
run_soft() {
  printf '+ %s\n' "$*"
  [ "$DRY" -eq 1 ] && return 0
  "$@" || echo "  (ignored: exit $?)"
}
