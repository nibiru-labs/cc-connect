package config

import (
	"strings"
	"testing"
)

const unifiedSessionFixture = `
[[projects]]
name = "sakura"
unified_session = true

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/s"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "x"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "y"

[[projects]]
name = "plain"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/p"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "z"
`

func TestLoad_ParsesUnifiedSession(t *testing.T) {
	cfg, err := Load(writeConfigFixture(t, unifiedSessionFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Projects[0].UnifiedSession {
		t.Fatal("unified_session = true not parsed")
	}
	if len(cfg.Projects[0].Platforms) != 2 {
		t.Fatalf("want 2 platforms under one project, got %d", len(cfg.Projects[0].Platforms))
	}
	if cfg.Projects[1].UnifiedSession {
		t.Fatal("unified_session must default to false")
	}
}

func TestLoad_RejectsUnifiedSessionWithMultiWorkspace(t *testing.T) {
	fx := `
[[projects]]
name = "mw"
mode = "multi-workspace"
base_dir = "/tmp/mw"
unified_session = true

[projects.agent]
type = "codex"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "x"
`
	_, err := Load(writeConfigFixture(t, fx))
	if err == nil || !strings.Contains(err.Error(), "unified_session") {
		t.Fatalf("expected unified_session/multi-workspace conflict error, got %v", err)
	}
}
