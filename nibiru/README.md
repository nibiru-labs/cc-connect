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

## Switching the inbox consumer (one consumer only!)

Two things must never read the same A2A inbox: `com.nibiru.sakura-runner` (python runner) and cc-connect's a2a platform.

```sh
nibiru/deploy/switch-to-a2a.sh --dry-run      # always look first
nibiru/deploy/switch-to-a2a.sh                # backup -> bootout runner -> reload cc-connect
nibiru/deploy/rollback-to-runner.sh --dry-run
nibiru/deploy/rollback-to-runner.sh           # restore config backup -> reload cc-connect -> bootstrap runner
```

Order matters and is enforced by the scripts: the old consumer is stopped before the new one starts.
Both scripts back up config/plist (`*.bak-a2a-<ts>`) and support `--dry-run` (print commands only).
`switch-to-a2a.sh` refuses to run if the config has no `type = "a2a"` platform or the binary is missing;
`rollback-to-runner.sh` refuses without a `config.toml.bak-a2a-*` backup (hand-edit instead).
The plists are only read, never modified.

## Tests

```sh
bash nibiru/deploy/deploy_test.sh    # dry-run behaviour of the three scripts
```

## Ops notes

- Logs: `~/.cc-connect/logs/cc-connect.log`; runner log: `~/.config/nibiru-a2a/runner.log`.
- Verify after switching: send a test A2A message from an allowed member; reply arrives, message leaves `read --pending`.
- Identity: sessions get env `NIBIRU_AGENT=sakura` (see the project's `[projects.agent.options.env]`).
