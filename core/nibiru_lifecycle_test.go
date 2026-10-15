package core

import (
	"strings"
	"testing"
	"time"
)

func TestShouldRotate(t *testing.T) {
	e := NewEngine("t", &controllableAgent{}, nil, "", LangEnglish)
	if e.shouldRotate(&ContextUsage{UsedTokens: 900, ContextWindow: 1000}) {
		t.Fatal("disabled by default")
	}
	e.SetRotateConfig(85, 0)
	if e.shouldRotate(&ContextUsage{UsedTokens: 849, ContextWindow: 1000}) {
		t.Fatal("84.9% must not rotate")
	}
	if !e.shouldRotate(&ContextUsage{UsedTokens: 850, ContextWindow: 1000}) {
		t.Fatal("85% must rotate")
	}
	if e.shouldRotate(nil) || e.shouldRotate(&ContextUsage{}) {
		t.Fatal("no usage must not rotate")
	}
	e.SetRotateConfig(0, 500)
	if !e.shouldRotate(&ContextUsage{UsedTokens: 500}) {
		t.Fatal("absolute fallback")
	}
}

func TestHandoffSaveAndConsumeOnce(t *testing.T) {
	e := NewEngine("t", &controllableAgent{}, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	if got := e.withHandoff("k:1", "hello"); got != "hello" {
		t.Fatalf("no handoff: %q", got)
	}
	if err := e.saveHandoff("k:1", "进度：做到 N6"); err != nil {
		t.Fatal(err)
	}
	got := e.withHandoff("k:1", "继续")
	if !strings.Contains(got, "进度：做到 N6") || !strings.HasSuffix(got, "继续") {
		t.Fatalf("handoff not injected: %q", got)
	}
	if again := e.withHandoff("k:1", "再来"); again != "再来" {
		t.Fatalf("handoff must be consumed once: %q", again)
	}
}

type rotateSession struct {
	*controllableAgentSession
	reply string
}

func (s *rotateSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	if strings.Contains(prompt, "交接摘要") {
		s.events <- Event{Type: EventText, Content: s.reply}
		s.events <- Event{Type: EventResult, Content: s.reply, Done: true}
	}
	return nil
}

func TestRunRotate_WritesHandoffAndResetsSession(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	base := newControllableSession("rot")
	sess := &rotateSession{controllableAgentSession: base, reply: "摘要：正在做 #836"}
	e := NewEngine("t", &controllableAgent{nextSession: base}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())

	key := "test:rot"
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("old-id", "claudecode")
	session.TryLock()

	done := make(chan bool, 1)
	go func() { done <- e.runRotate(state, session, e.sessions, key, p, "ctx", 0) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("runRotate returned false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRotate hung")
	}
	if session.GetAgentSessionID() != "" {
		t.Fatal("agent session id must be cleared so the next message starts fresh")
	}
	if got := e.withHandoff(key, "next"); !strings.Contains(got, "正在做 #836") {
		t.Fatalf("summary not carried to the next prompt: %q", got)
	}
}

func TestRunRotate_SkippedWhenMessagesQueued(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("rot2")
	e := NewEngine("t", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	state.pendingMessages = append(state.pendingMessages, queuedMessage{})
	session := e.sessions.GetOrCreateActive("test:rot2")
	if e.runRotate(state, session, e.sessions, "test:rot2", p, "ctx", 0) {
		t.Fatal("must not rotate while messages are queued")
	}
}
