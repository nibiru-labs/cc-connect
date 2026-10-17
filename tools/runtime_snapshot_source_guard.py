#!/usr/bin/env python3
"""Read-only provenance check for the installed-base runtime backport."""
import json
from pathlib import Path
import subprocess

BASE = "1ffc86d604eaf48fbcb5d2b000971d2222a1092d"
RUNTIME_SOURCE = "efad8d569ca3426b24fcdab6a3b7dbfe00f88bf6"
RUNTIME_FILES = {
    "agent/codex/appserver_session.go",
    "agent/codex/runtime_snapshot.go",
    "agent/codex/runtime_snapshot_test.go",
    "core/api.go",
    "core/runtime_snapshot.go",
    "core/runtime_snapshot_test.go",
    "docs/runtime-snapshot.md",
}
INTEGRATED_SOURCE_BASE_COMMIT = "9540a1b087dc19b423606ecaae11052d0ca8a197"
INTEGRATED_SOURCE_BLOBS = {
    # Pin PR20's engine plus only PR16's relay inventory lifecycle delta.
    # Do not replace this combined file with either source file wholesale.
    "core/engine.go": "c2ebd39192daaac11aac3a13e40dd1ada03b6ef9",
    # The engine test remains byte-identical to the reviewed PR20 source.
    "core/engine_test.go": "71de554a5860c3df1a7146c27acf7fb59227e764",
}
ALLOWED = RUNTIME_FILES | set(INTEGRATED_SOURCE_BLOBS) | {
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
    for path, expected_blob in sorted(INTEGRATED_SOURCE_BLOBS.items()):
        actual_blob = git("rev-parse", "HEAD:" + path).decode().strip()
        if actual_blob != expected_blob:
            composition = (
                "PR20 base " + INTEGRATED_SOURCE_BASE_COMMIT
                + " plus PR16 relay delta " + RUNTIME_SOURCE
                if path == "core/engine.go"
                else "PR20 source " + INTEGRATED_SOURCE_BASE_COMMIT
            )
            raise SystemExit(
                "Integrated source differs from pinned composition "
                + composition
                + ": "
                + path
            )
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
