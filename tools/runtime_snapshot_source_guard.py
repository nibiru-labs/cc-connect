#!/usr/bin/env python3
"""Read-only provenance check for the installed-base runtime backport."""
import json
from pathlib import Path
import subprocess

BASE = "1ffc86d604eaf48fbcb5d2b000971d2222a1092d"
RUNTIME_SOURCE = "8bc232f6577a8178a42eedd51ab153c6692c20f5"
RUNTIME_FILES = {
    "agent/codex/appserver_session.go",
    "agent/codex/runtime_snapshot.go",
    "agent/codex/runtime_snapshot_test.go",
    "core/api.go",
    "core/runtime_snapshot.go",
    "core/runtime_snapshot_test.go",
    "docs/runtime-snapshot.md",
}
ALLOWED = RUNTIME_FILES | {
    "spec/1256-installed-backport.md",
    "docs/runtime-backport-release.md",
    "tools/runtime_snapshot_source_guard.py",
}


def git(*args):
    return subprocess.check_output(["git", *args], cwd=Path(__file__).resolve().parents[1])


def main():
    git("merge-base", "--is-ancestor", BASE, "HEAD")
    changed = set(git("diff", "--no-renames", "--name-only", BASE, "HEAD").decode().splitlines())
    unexpected = sorted(changed - ALLOWED)
    if unexpected:
        raise SystemExit("Unexpected source changes: " + ", ".join(unexpected))
    for path in sorted(RUNTIME_FILES):
        if git("show", "HEAD:" + path) != git("show", RUNTIME_SOURCE + ":" + path):
            raise SystemExit("Runtime patch differs from reviewed source: " + path)
    if git("status", "--porcelain", "--untracked-files=normal").strip():
        raise SystemExit("Build source must be clean (including untracked files)")
    # This checks every tracked baseline file outside the explicit allowlist,
    # including the entire A2A plugin/lifecycle/config/cron/deployment trees.
    print(json.dumps({"installed_base": BASE, "runtime_source": RUNTIME_SOURCE,
                      "head": git("rev-parse", "HEAD").decode().strip(),
                      "changed_files": sorted(changed),
                      "other_baseline_files": "unchanged"}, indent=2))


if __name__ == "__main__":
    main()
