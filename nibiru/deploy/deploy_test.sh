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
has "switch: keeps a2a config backup (forward ref)" "$out" "config.toml.bak-a2a-"
b=${out%%com.cc-connect.service*}
case "$b" in *bootout*sakura-runner*) ok "switch: runner stopped first";; *) bad "switch: order";; esac

out=$("$D/rollback-to-runner.sh" --dry-run 2>&1)
has "rollback: bootout cc"   "$out" "launchctl bootout gui/$(id -u)/com.cc-connect.service"
has "rollback: bootstrap runner" "$out" "launchctl bootstrap gui/$(id -u)"
has "rollback: runner plist" "$out" "com.nibiru.sakura-runner.plist"
hasnt "rollback: dry-run no exec" "$out" "EXEC:"
has "rollback: restores baseline" "$out" "config.toml.runner-baseline"

# Non-dry refusals: every case must stop BEFORE any launchctl call (no real launchd access).
mkdir -p "$HOME/.cc-connect"
A2A_CFG='[[projects]]
name = "a2a"
[[projects.platforms]]
type = "a2a"
'
CLEAN_CFG='[[projects]]
name = "dingtalk"
'
refuse() { # name, expected substring, command...
  n=$1; want=$2; shift 2
  out=$("$@" 2>&1); rc=$?
  if [ $rc -ne 0 ] && case "$out" in *"$want"*) true;; *) false;; esac && case "$out" in *"+ launchctl"*) false;; *) true;; esac; then
    ok "$n"
  else
    bad "$n (rc=$rc out=$out)"
  fi
}
rm -f "$HOME/.cc-connect/config.toml" "$HOME/.cc-connect/config.toml.runner-baseline"
refuse "switch: refuses without config" "missing" "$D/switch-to-a2a.sh"
printf '%s' "$A2A_CFG" > "$HOME/.cc-connect/config.toml"
refuse "switch: refuses without baseline" "missing" "$D/switch-to-a2a.sh"
printf '%s' "$A2A_CFG" > "$HOME/.cc-connect/config.toml.runner-baseline"
refuse "switch: refuses when baseline has a2a" "already contains an a2a" "$D/switch-to-a2a.sh"
printf '%s' "$CLEAN_CFG" > "$HOME/.cc-connect/config.toml.runner-baseline"
refuse "switch: refuses without binary" "missing binary" "$D/switch-to-a2a.sh"
rm -f "$HOME/.cc-connect/config.toml.runner-baseline"
refuse "rollback: refuses without baseline" "missing" "$D/rollback-to-runner.sh"
printf '%s' "$A2A_CFG" > "$HOME/.cc-connect/config.toml.runner-baseline"
refuse "rollback: refuses when baseline has a2a" "contains an a2a" "$D/rollback-to-runner.sh"

# has_a2a / stop_service helpers
. "$D/lib.sh"
has_a2a "$HOME/.cc-connect/config.toml" && ok "has_a2a: detects a2a" || bad "has_a2a: detects a2a"
printf '%s' "$CLEAN_CFG" > "$HOME/.cc-connect/clean.toml"
has_a2a "$HOME/.cc-connect/clean.toml" && bad "has_a2a: clean config" || ok "has_a2a: clean config"

for s in build switch-to-a2a rollback-to-runner; do
  "$D/$s.sh" --bogus >/dev/null 2>&1 && bad "$s: accepts bogus flag" || ok "$s: rejects bogus flag"
done
exit $fail
