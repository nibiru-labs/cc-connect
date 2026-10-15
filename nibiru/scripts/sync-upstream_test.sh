#!/usr/bin/env bash
# Self-test for sync-upstream.sh using throw-away git repos (no network, no real remotes).
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC="$HERE/sync-upstream.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
export SYNC_TEST_CMD=true SYNC_BUILD_CMD=true
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "ok   - $1"; }
bad() { fail=$((fail+1)); echo "FAIL - $1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# setup <dir>: bare upstream + bare origin + work clone with branches main/upstream/nibiru
setup() {
  local d="$1"; mkdir -p "$d"
  git init -q --bare -b main "$d/up.git"
  git init -q --bare -b main "$d/origin.git"
  git clone -q "$d/up.git" "$d/seed" 2>/dev/null
  ( cd "$d/seed" && git checkout -q -b main 2>/dev/null; echo base > f.txt; echo one > g.txt
    git add . && git commit -q -m base && git push -q origin main
    git push -q "$d/origin.git" main:main main:upstream main:nibiru
    echo nib > nibiru-only.txt; git add . && git commit -q -m "nibiru change"
    git push -q "$d/origin.git" HEAD:nibiru )
  git clone -q "$d/origin.git" "$d/work" 2>/dev/null
  ( cd "$d/work" && git remote add upstream "$d/up.git" && git config user.name t && git config user.email t@t )
}
upstream_commit() { # dir file content msg
  ( cd "$1/seed" && git checkout -q main 2>/dev/null; git reset -q --hard origin/main
    echo "$3" > "$2"; git add . && git commit -q -m "$4" && git push -q origin main )
}
rev() { git --git-dir="$1" rev-parse "$2"; }

# 1. up to date
D="$TMP/t1"; setup "$D"
out="$(cd "$D/work" && "$SYNC" 2>&1)"; rc=$?
check "up-to-date exits 0" "[ $rc -eq 0 ]"
check "up-to-date reports it" "grep -q 'up to date' <<<\"\$out\""

# 2. dry-run reports new commits, pushes nothing
D="$TMP/t2"; setup "$D"; upstream_commit "$D" new.txt x "upstream feat A"
before_n="$(rev "$D/origin.git" nibiru)"; before_u="$(rev "$D/origin.git" upstream)"
out="$(cd "$D/work" && "$SYNC" --dry-run 2>&1)"; rc=$?
check "dry-run exits 0" "[ $rc -eq 0 ]"
check "dry-run lists upstream commit" "grep -q 'upstream feat A' <<<\"\$out\""
check "dry-run says dry-run" "grep -qi 'dry-run' <<<\"\$out\""
check "dry-run leaves origin nibiru" "[ \"\$(rev $D/origin.git nibiru)\" = \"$before_n\" ]"
check "dry-run leaves origin upstream" "[ \"\$(rev $D/origin.git upstream)\" = \"$before_u\" ]"
check "dry-run leaves no worktrees" "[ \"\$(cd $D/work && git worktree list | wc -l)\" -eq 1 ]"

# 3. real run pushes (to temp origin)
D="$TMP/t3"; setup "$D"; upstream_commit "$D" new.txt x "upstream feat B"
out="$(cd "$D/work" && "$SYNC" 2>&1)"; rc=$?
check "sync exits 0" "[ $rc -eq 0 ]"
check "origin/upstream == upstream/main" "[ \"\$(rev $D/origin.git upstream)\" = \"\$(rev $D/up.git main)\" ]"
check "origin/nibiru contains upstream main" "git --git-dir=$D/origin.git merge-base --is-ancestor \$(rev $D/up.git main) nibiru"
check "origin/nibiru keeps nibiru change" "git --git-dir=$D/origin.git cat-file -e nibiru:nibiru-only.txt"

# 4. conflict stops, nothing pushed
D="$TMP/t4"; setup "$D"
( cd "$D/seed" && git checkout -q main; git reset -q --hard origin/main; echo up > f.txt; git add . && git commit -q -m "up edit f" && git push -q origin main )
( cd "$D/seed" && git fetch -q "$D/origin.git" nibiru && git checkout -q -B nibtmp FETCH_HEAD && echo nib > f.txt && git add . && git commit -q -m "nib edit f" && git push -q "$D/origin.git" HEAD:nibiru )
before_n="$(rev "$D/origin.git" nibiru)"; before_u="$(rev "$D/origin.git" upstream)"
out="$(cd "$D/work" && "$SYNC" 2>&1)"; rc=$?
check "conflict exits 2" "[ $rc -eq 2 ]"
check "conflict names file" "grep -q 'f.txt' <<<\"\$out\""
check "conflict pushes nothing (nibiru)" "[ \"\$(rev $D/origin.git nibiru)\" = \"$before_n\" ]"
check "conflict pushes nothing (upstream)" "[ \"\$(rev $D/origin.git upstream)\" = \"$before_u\" ]"
check "conflict leaves no worktrees" "[ \"\$(cd $D/work && git worktree list | wc -l)\" -eq 1 ]"

# 5. failing tests block push
D="$TMP/t5"; setup "$D"; upstream_commit "$D" new.txt x "upstream feat C"
before_n="$(rev "$D/origin.git" nibiru)"
out="$(cd "$D/work" && SYNC_TEST_CMD=false "$SYNC" 2>&1)"; rc=$?
check "test failure exits 3" "[ $rc -eq 3 ]"
check "test failure pushes nothing" "[ \"\$(rev $D/origin.git nibiru)\" = \"$before_n\" ]"

# 6. diverged mirror (origin/upstream not ancestor of upstream/main) stops
D="$TMP/t6"; setup "$D"; upstream_commit "$D" new.txt x "upstream feat D"
( cd "$D/seed" && git fetch -q "$D/origin.git" nibiru && git checkout -q -B stray FETCH_HEAD && git push -q -f "$D/origin.git" HEAD:upstream )
out="$(cd "$D/work" && "$SYNC" 2>&1)"; rc=$?
check "diverged mirror exits 4" "[ $rc -eq 4 ]"

echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
