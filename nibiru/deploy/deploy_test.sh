#!/usr/bin/env bash
# Tests for nibiru/deploy scripts. Only exercises --dry-run; never touches launchd.
set -u
D="$(cd "$(dirname "$0")" && pwd)"
fail=0
ok()  { echo "ok   - $1"; }
bad() { echo "FAIL - $1"; fail=1; }
has() { case "$2" in *"$3"*) ok "$1";; *) bad "$1 (missing: $3)"; echo "$2" | sed 's/^/     /';; esac; }
hasnt() { case "$2" in *"$3"*) bad "$1 (unexpected: $3)";; *) ok "$1";; esac; }
export HOME="$(mktemp -d)"; trap 'rm -rf "$HOME"' EXIT

out=$("$D/build.sh" --dry-run --tag v9.9.9 2>&1)
has "build: tags"    "$out" "-tags no_web"
has "build: version" "$out" "main.version=v9.9.9-nibiru"
has "build: output"  "$out" "$HOME/.local/nibiru/bin/cc-connect"
has "build: backup"  "$out" "cc-connect.bak-"
[ -e "$HOME/.local" ] && bad "build: dry-run wrote files" || ok "build: dry-run wrote nothing"

out=$("$D/switch-to-a2a.sh" --dry-run 2>&1)
has "switch: bootout runner" "$out" "launchctl bootout gui/$(id -u)/com.nibiru.sakura-runner"
has "switch: bootstrap cc"   "$out" "com.cc-connect.service"
b=${out%%com.cc-connect.service*}
case "$b" in *bootout*sakura-runner*) ok "switch: runner stopped first";; *) bad "switch: order";; esac

out=$("$D/rollback-to-runner.sh" --dry-run 2>&1)
has "rollback: bootout cc"   "$out" "launchctl bootout gui/$(id -u)/com.cc-connect.service"
has "rollback: bootstrap runner" "$out" "launchctl bootstrap gui/$(id -u)"
has "rollback: runner plist" "$out" "com.nibiru.sakura-runner.plist"
hasnt "rollback: dry-run no exec" "$out" "EXEC:"

for s in build switch-to-a2a rollback-to-runner; do
  "$D/$s.sh" --bogus >/dev/null 2>&1 && bad "$s: accepts bogus flag" || ok "$s: rejects bogus flag"
done
exit $fail
