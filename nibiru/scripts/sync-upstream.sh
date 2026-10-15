#!/usr/bin/env bash
# sync-upstream.sh — merge upstream cc-connect into the nibiru branch (nibiru-labs/nibiru#836, L5).
#
# Steps: fetch -> fast-forward origin/upstream to upstream/main -> merge into nibiru in a
# throw-away worktree (conflict => abort + report) -> test -> build (-tags no_web) -> push.
# Nothing is pushed unless every step passes; --dry-run never pushes.
#
# Usage: nibiru/scripts/sync-upstream.sh [--dry-run] [-h]
# Exit:  0 ok / up to date, 1 usage or git error, 2 merge conflict,
#        3 tests failed, 4 mirror branch diverged, 5 build failed
# Env (all optional):
#   UPSTREAM_REMOTE=upstream ORIGIN_REMOTE=origin UPSTREAM_BRANCH=main
#   MIRROR_BRANCH=upstream DEV_BRANCH=nibiru
#   SYNC_TEST_CMD   override test command (default: go test on a2a/nibiru/core packages)
#   SYNC_BUILD_CMD  override build command (default: go build -tags no_web into a temp dir)
set -u

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
ORIGIN_REMOTE="${ORIGIN_REMOTE:-origin}"
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"
MIRROR_BRANCH="${MIRROR_BRANCH:-upstream}"
DEV_BRANCH="${DEV_BRANCH:-nibiru}"
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

say() { echo "[sync-upstream] $*"; }
die() { local rc="$1"; shift; say "ERROR: $*"; exit "$rc"; }

git rev-parse --git-dir >/dev/null 2>&1 || die 1 "not inside a git repository"
REPO="$(git rev-parse --show-toplevel)"
cd "$REPO" || die 1 "cannot cd to $REPO"

WT=""
OUT=""
cleanup() {
  [ -n "$WT" ] && git -C "$REPO" worktree remove --force "$WT" >/dev/null 2>&1
  [ -n "$OUT" ] && rm -rf "$OUT"
  [ -n "$WT" ] && rm -rf "$WT"
}
trap cleanup EXIT

U="$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
M="$ORIGIN_REMOTE/$MIRROR_BRANCH"
D="$ORIGIN_REMOTE/$DEV_BRANCH"

say "fetching $UPSTREAM_REMOTE and $ORIGIN_REMOTE"
git fetch -q "$UPSTREAM_REMOTE" || die 1 "fetch $UPSTREAM_REMOTE failed"
git fetch -q "$ORIGIN_REMOTE" || die 1 "fetch $ORIGIN_REMOTE failed"
git rev-parse -q --verify "$U^{commit}" >/dev/null || die 1 "$U not found"
git rev-parse -q --verify "$D^{commit}" >/dev/null || die 1 "$D not found"

# 1. mirror branch must fast-forward
MIRROR_PUSH=1
if git rev-parse -q --verify "$M^{commit}" >/dev/null; then
  git merge-base --is-ancestor "$M" "$U" \
    || die 4 "$M is not an ancestor of $U (mirror diverged); fix manually"
  [ "$(git rev-parse "$M")" = "$(git rev-parse "$U")" ] && MIRROR_PUSH=0
fi

# 2. what is new for nibiru?
NEW_COUNT="$(git rev-list --count "$D..$U")"
say "upstream head: $(git rev-parse --short "$U"); new commits vs $D: $NEW_COUNT"
if [ "$NEW_COUNT" -eq 0 ]; then
  if [ "$MIRROR_PUSH" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
    git push -q "$ORIGIN_REMOTE" "$U:refs/heads/$MIRROR_BRANCH" || die 1 "mirror push failed"
    say "mirror $MIRROR_BRANCH fast-forwarded"
  fi
  say "$DEV_BRANCH is up to date with upstream"
  exit 0
fi
git log --oneline --no-merges "$D..$U" | head -50 | sed 's/^/  + /'
[ "$NEW_COUNT" -gt 50 ] && say "  (... showing first 50 non-merge commits of $NEW_COUNT)"

# 3. merge in a throw-away worktree
WT="$(mktemp -d)"; rm -rf "$WT"
git worktree add -q --detach "$WT" "$D" || die 1 "worktree add failed"
if ! git -C "$WT" merge --no-ff -q -m "Merge upstream/$UPSTREAM_BRANCH into $DEV_BRANCH" "$U" >/tmp/sync-merge.$$ 2>&1; then
  say "MERGE CONFLICT — files:"
  git -C "$WT" diff --name-only --diff-filter=U | sed 's/^/  ! /'
  git -C "$WT" merge --abort >/dev/null 2>&1
  rm -f /tmp/sync-merge.$$
  die 2 "merge of $U into $D conflicted; nothing pushed"
fi
rm -f /tmp/sync-merge.$$
MERGED="$(git -C "$WT" rev-parse HEAD)"
say "merged cleanly -> ${MERGED:0:9}"

# 4. tests
say "running tests"
if [ -n "${SYNC_TEST_CMD:-}" ]; then
  ( cd "$WT" && eval "$SYNC_TEST_CMD" ) || die 3 "tests failed; nothing pushed"
else
  ( cd "$WT" && go test ./platform/a2a/... ./nibiru/... ./core/... ) || die 3 "tests failed; nothing pushed"
fi

# 5. build
OUT="$(mktemp -d)"
say "building (-tags no_web) into temp dir"
if [ -n "${SYNC_BUILD_CMD:-}" ]; then
  ( cd "$WT" && eval "$SYNC_BUILD_CMD" ) || die 5 "build failed; nothing pushed"
else
  ( cd "$WT" && go build -tags no_web -o "$OUT/cc-connect" ./cmd/cc-connect ) || die 5 "build failed; nothing pushed"
fi

# 6. report / push
say "REPORT: $NEW_COUNT upstream commit(s) merged; tests ok; build ok; result ${MERGED:0:9}"
if [ "$DRY_RUN" -eq 1 ]; then
  say "dry-run: would push $MERGED -> $ORIGIN_REMOTE/$DEV_BRANCH and $U -> $ORIGIN_REMOTE/$MIRROR_BRANCH; not pushing"
  exit 0
fi
git push -q "$ORIGIN_REMOTE" "$U:refs/heads/$MIRROR_BRANCH" "$MERGED:refs/heads/$DEV_BRANCH" \
  || die 1 "push failed"
say "pushed $MIRROR_BRANCH and $DEV_BRANCH to $ORIGIN_REMOTE"
