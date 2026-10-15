package core

// nibiru#836 lifecycle: rotate a long-running session once its context passes a
// threshold. The old session writes a handoff summary, the engine starts a fresh
// agent session and prepends the summary to its first prompt. Memory across
// rotations comes only from the summary and the repository.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	handoffDirName        = "nibiru-handoff"
	handoffMaxRunes       = 4000
	handoffTurnTimeout    = 5 * time.Minute
	handoffPromptTemplate = "上下文即将用尽，本会话会被轮换成新会话。请不要调用任何工具，直接输出一份交接摘要（不超过 1500 字），" +
		"包含：1) 正在做的事与进度；2) 已做的决定及原因；3) 未完成事项与下一步；4) 关键 issue / PR / 文件路径；5) 需要提醒用户的事。只输出摘要正文。"
)

var handoffKeySanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// nibiruLifecycle holds rotate settings. Zero value = rotation disabled.
type nibiruLifecycle struct {
	rotatePercent   int // rotate when used*100 >= window*percent (needs ContextWindow)
	rotateMaxTokens int // absolute fallback when the window is unknown
	compressPercent int // like auto_compress, but as a percentage of the window
}

// SetRotateConfig enables rotation. percent<=0 and maxTokens<=0 disables it.
func (e *Engine) SetRotateConfig(percent, maxTokens int) {
	e.lifecycle.rotatePercent = percent
	e.lifecycle.rotateMaxTokens = maxTokens
}

// SetCompressPercent makes auto-compress also trigger at percent of the window.
func (e *Engine) SetCompressPercent(percent int) {
	e.lifecycle.compressPercent = percent
}

func usagePercentReached(u *ContextUsage, percent int) bool {
	return u != nil && percent > 0 && u.ContextWindow > 0 && u.UsedTokens > 0 &&
		u.UsedTokens*100 >= u.ContextWindow*percent
}

// shouldRotate decides from exact, agent-reported usage only (never a heuristic).
func (e *Engine) shouldRotate(u *ContextUsage) bool {
	if u == nil || u.UsedTokens <= 0 {
		return false
	}
	lc := e.lifecycle
	if usagePercentReached(u, lc.rotatePercent) {
		return true
	}
	return lc.rotateMaxTokens > 0 && u.UsedTokens >= lc.rotateMaxTokens
}

func (e *Engine) handoffDir() string {
	if e.dataDir == "" {
		return ""
	}
	return filepath.Join(e.dataDir, handoffDirName)
}

func handoffFileBase(key string) string {
	return handoffKeySanitizer.ReplaceAllString(key, "_")
}

func handoffTruncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…[已截断]"
}

// saveHandoff writes the summary as the pending handoff for key and keeps a dated copy.
func (e *Engine) saveHandoff(key, summary string) error {
	dir := e.handoffDir()
	if dir == "" {
		return fmt.Errorf("handoff: no data dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	summary = handoffTruncate(strings.TrimSpace(summary), handoffMaxRunes)
	base := handoffFileBase(key)
	archive := filepath.Join(dir, fmt.Sprintf("%s.%s.md", base, time.Now().Format("20060102-150405")))
	if err := os.WriteFile(archive, []byte(summary+"\n"), 0o600); err != nil {
		return err
	}
	tmp := filepath.Join(dir, base+".pending.md.tmp")
	if err := os.WriteFile(tmp, []byte(summary+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, base+".pending.md"))
}

// peekHandoff returns the pending handoff for key without clearing it.
func (e *Engine) peekHandoff(key string) (string, string) {
	dir := e.handoffDir()
	if dir == "" {
		return "", ""
	}
	p := filepath.Join(dir, handoffFileBase(key)+".pending.md")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(string(b)), p
}

// discardPendingHandoff drops the pending handoff for key (the dated archive stays).
func (e *Engine) discardPendingHandoff(key string) {
	if _, p := e.peekHandoff(key); p != "" {
		_ = os.Remove(p)
	}
}

// withHandoff prepends a pending handoff (if any) to the first prompt of a fresh session.
// The handoff stays on disk until the caller invokes ack after a successful Send, so a
// failed send keeps it for the next attempt (nibiru#836 review P2).
func (e *Engine) withHandoff(key, prompt string) (string, func()) {
	h, p := e.peekHandoff(key)
	if h == "" {
		return prompt, func() {}
	}
	slog.Info("handoff: injected into new session", "session_key", key, "runes", utf8.RuneCountInString(h))
	ack := func() {
		// Remove only the file we injected; a newer handoff written meanwhile is kept.
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == h {
			_ = os.Remove(p)
		}
	}
	return "【交接摘要：上一段会话因上下文用尽被轮换。你的记忆只来自这份摘要和仓库/issue，其余以实际查证为准】\n" +
		h + "\n【交接摘要结束】\n\n" + prompt, ack
}

// historyFallbackSummary builds a summary from the transcript when the model gave none.
func historyFallbackSummary(hist []HistoryEntry) string {
	if len(hist) > 12 {
		hist = hist[len(hist)-12:]
	}
	var b strings.Builder
	b.WriteString("（模型未能生成摘要，以下为最近对话节选）\n")
	for _, h := range hist {
		b.WriteString(fmt.Sprintf("- %s: %s\n", h.Role, handoffTruncate(strings.TrimSpace(h.Content), 300)))
	}
	return b.String()
}

// runRotate runs the handoff turn, stores the summary and resets the session.
// It returns false (and does nothing) when rotation must wait, e.g. queued messages.
// The caller must hold the session busy lock; runRotate releases it when it returns true.
func (e *Engine) runRotate(state *interactiveState, session *Session, sessions *SessionManager, key string, p Platform, replyCtx any, lockGen uint64) bool {
	state.mu.Lock()
	queued := len(state.pendingMessages)
	as := state.agentSession
	state.mu.Unlock()
	if queued > 0 || as == nil || !as.Alive() {
		return false
	}

	e.stopUnsolicitedReader(state)
	drainEvents(as.Events())
	if err := as.Send(handoffPromptTemplate, "", nil, nil); err != nil {
		slog.Warn("rotate: handoff prompt failed, skipping rotation", "session_key", key, "error", err)
		return false
	}

	var parts []string
	timer := time.NewTimer(handoffTurnTimeout)
	defer timer.Stop()
	events := as.Events()
	stopCh := state.stopSignal()
collect:
	for {
		select {
		case <-stopCh:
			return false
		case <-e.ctx.Done():
			return false
		case <-timer.C:
			slog.Warn("rotate: handoff turn timed out", "session_key", key)
			break collect
		case ev, ok := <-events:
			if !ok {
				break collect
			}
			switch ev.Type {
			case EventText:
				parts = append(parts, ev.Content)
			case EventPermissionRequest:
				_ = as.RespondPermission(ev.RequestID, PermissionResult{Behavior: "deny"})
			case EventResult:
				if ev.Content != "" && len(parts) == 0 {
					parts = append(parts, ev.Content)
				}
				break collect
			case EventError:
				break collect
			}
		}
	}

	// Messages that arrived while the summary was being written must not be dropped by
	// the cleanup below: keep the current session, let the caller drain the queue and
	// retry rotation after a later turn (nibiru#836 review P1).
	if n := e.queuedCount(state); n > 0 {
		slog.Info("rotate: messages queued during handoff, postponing rotation", "session_key", key, "queued", n)
		return false
	}

	summary := strings.TrimSpace(strings.Join(parts, ""))
	if summary == "" {
		summary = historyFallbackSummary(session.GetHistory(0))
	}
	if err := e.saveHandoff(key, summary); err != nil {
		// Without a saved summary a rotation would lose memory: keep the old session.
		slog.Error("rotate: cannot save handoff, keeping current session", "session_key", key, "error", err)
		return false
	}
	if n := e.queuedCount(state); n > 0 {
		e.discardPendingHandoff(key)
		slog.Info("rotate: messages queued while saving handoff, postponing rotation", "session_key", key, "queued", n)
		return false
	}

	e.send(p, replyCtx, "上下文已接近上限，已写交接摘要并轮换为新会话；下一条消息会带着摘要继续。")
	e.cleanupInteractiveState(key, state)
	session.SetAgentSessionID("", "")
	session.ClearHistory()
	sessions.Save()
	session.Unlock(lockGen)
	slog.Info("rotate: session rotated with handoff", "session_key", key)
	return true
}

func (e *Engine) queuedCount(state *interactiveState) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.pendingMessages)
}

// HasPlatformForSession reports whether sessionKey (optionally workspace-prefixed)
// belongs to a platform this engine has. Used to reject cron jobs bound to
// platforms that are not configured (nibiru#836: three jobs on a removed "weixin").
func (e *Engine) HasPlatformForSession(sessionKey string) bool {
	for _, p := range e.platforms {
		name := p.Name()
		if strings.HasPrefix(sessionKey, name+":") || strings.Contains(sessionKey, ":"+name+":") {
			return true
		}
	}
	return false
}
