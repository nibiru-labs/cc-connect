# Nibiru additions to cc-connect

Fork of cc-connect used by Nibiru (nibiru-labs/nibiru#836, "plan B": one unified message dispatcher).
Rule: everything Nibiru-specific is **additive** (new files); upstream files are not edited, so
`nibiru/scripts/sync-upstream.sh` can merge upstream cleanly. Branch `nibiru` is ours, `upstream` mirrors upstream main.

## What lives where

| Path | Purpose |
|---|---|
| `platform/a2a/` | A2A as a chat platform: polls `a2a read --pending`, replies via `a2a send`, acks after the final reply succeeds, answers pong in the platform layer, roster filter (`allow_from`) |
| `cmd/cc-connect/plugin_platform_a2a.go` | build-tagged plugin registration |
| `nibiru/steward/`, `cmd/nibiru-steward/` | M3: rotate a session at >=85% usage with a handoff summary |
| `nibiru/status/`, `cmd/nibiru-status/` | unified context: sakura status summary |
| `nibiru/scripts/` | upstream sync |
| `nibiru/deploy/` | build, config sample, runner<->a2a switch/rollback (this document) |
| `nibiru/e2e/` | end-to-end tests with a fake A2A server |

## Build

```sh
nibiru/deploy/build.sh --dry-run     # print what would happen
nibiru/deploy/build.sh [--tag v1.5.1-beta.3]
```

`go build -tags no_web`, version string `<tag>-nibiru` (`cc-connect --version`), output
`~/.local/nibiru/bin/cc-connect`. The previous binary is copied to `cc-connect.bak-<timestamp>` first;
the new one is built to `cc-connect.new` and moved into place, so a failed build keeps the old binary.
The script does not restart the service.

## Config

`nibiru/deploy/config.a2a.example.toml` is an a2a project sample: `allow_from` roster
(default `grok,dot,claude-cloud,hermes`), model `sonnet`, work dir `~/workspace/nibiru`.
A2A projects default to sonnet; on DingTalk projects the user switches with `/model`.
Merge it into `~/.cc-connect/config.toml` by hand (secrets stay in `~/.cc-connect/dingtalk.env`; never commit them).

## Unified session (nibiru#966)

`unified_session = true` (project level, default false) merges every platform/conversation of the project
into one agent session: key `unified:<project>`, messages serialised through the existing busy queue, per-message
reply ctx kept (replies return to their source), prompts prefixed with `[cc-connect source platform= sender_id=
sender_name= conversation= msg_id=]`. Slash commands/permission answers are not tagged. Sample:
`nibiru/deploy/config.unified.example.toml` (DingTalk + WeChat + A2A in one project). Code: `core/unified_session.go`.
Not supported together with `mode = "multi-workspace"` (config load rejects the combination).
Note: a message arriving while a turn runs gets the stock "queued" notice on its own platform (on A2A that is one extra message).

## Switching the inbox consumer (one consumer only!)

Two things must never read the same A2A inbox: `com.nibiru.sakura-runner` (python runner) and cc-connect's a2a platform.

Before merging the a2a project into `~/.cc-connect/config.toml`, save the pre-a2a config as the rollback baseline (it must NOT contain `type = "a2a"`):

```sh
cp -p ~/.cc-connect/config.toml ~/.cc-connect/config.toml.runner-baseline
```

```sh
nibiru/deploy/switch-to-a2a.sh --dry-run      # always look first
nibiru/deploy/switch-to-a2a.sh                # bootout runner (confirmed unloaded) -> reload cc-connect
nibiru/deploy/rollback-to-runner.sh --dry-run
nibiru/deploy/rollback-to-runner.sh           # confirm cc-connect unloaded -> restore baseline -> bootstrap cc-connect -> bootstrap runner
```

Order matters and is enforced by the scripts: the old consumer is confirmed unloaded before the new one starts.
A failed bootout is never ignored; the script aborts and leaves the other consumer alone.
`switch-to-a2a.sh` refuses to run if the config has no `type = "a2a"` platform, the baseline is missing or
already contains a2a, or the binary is missing. `rollback-to-runner.sh` refuses without a baseline that
contains no a2a platform, and refuses if the runner is already loaded.
Both scripts support `--dry-run` (print commands only). `switch-to-a2a.sh` keeps `*.bak-a2a-<ts>` backups of the config and plist.
The plists are only read, never modified.

## Tests

```sh
bash nibiru/deploy/deploy_test.sh    # dry-run behaviour of the three scripts
```

## Ops notes

- Logs: `~/.cc-connect/logs/cc-connect.log`; runner log: `~/.config/nibiru-a2a/runner.log`.
- Verify after switching: send a test A2A message from an allowed member; reply arrives, message leaves `read --pending`.
- Identity: sessions get env `NIBIRU_AGENT=sakura` (see the project's `[projects.agent.options.env]`).
