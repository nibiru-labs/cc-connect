#!/usr/bin/env bash
# Build the nibiru fork of cc-connect into ~/.local/nibiru/bin/cc-connect.
# Usage: build.sh [--dry-run] [--tag <tag>]   (tag defaults to latest git tag)
set -euo pipefail
cd "$(dirname "$0")/../.."
. nibiru/deploy/lib.sh

TAG=""
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) [ $# -ge 2 ] || { echo "--tag needs a value" >&2; exit 2; }; TAG="$2"; shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
parse_dry_run ${args[@]+"${args[@]}"}
[ -n "$TAG" ] || TAG="$(git describe --tags --abbrev=0 2>/dev/null || echo dev)"

VERSION="${TAG}-nibiru"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
BUILT="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
OUT_DIR="$(dirname "$BIN")"
TMP="$BIN.new"

echo "version: $VERSION  commit: $COMMIT"
run mkdir -p "$OUT_DIR"
run go build -tags no_web \
  -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildTime=$BUILT" \
  -o "$TMP" ./cmd/cc-connect
# Build to a temp file first so a failed build never clobbers the running binary.
if [ "$DRY" -eq 1 ] || [ -e "$BIN" ]; then
  run cp -p "$BIN" "$BIN.bak-$STAMP"
fi
run mv -f "$TMP" "$BIN"
echo "installed: $BIN (restart cc-connect to pick it up; this script does not)"
