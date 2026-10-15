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
	if got, _ := e.withHandoff("k:1", "hello"); got != "hello" {
		t.Fatalf("no handoff: %q", got)
	}
	if err := e.saveHandoff("k:1", "进度：做到 N6"); err != nil {
		t.Fatal(err)
	}
	got, ack := e.withHandoff("k:1", "继续")
	if !strings.Contains(got, "进度：做到 N6") || !strings.HasSuffix(got, "继续") {
		t.Fatalf("handoff not injected: %q", got)
	}
	ack()
	if again, _ := e.withHandoff("k:1", "再来"); again != "再来" {
		t.Fatalf("handoff must be consumed once: %q", again)
	}
}

// Send failed → the handoff must survive for the next attempt (review P2).
func TestHandoffKeptWhenSendNotAcked(t *testing.T) {
	e := NewEngine("t", &controllableAgent{}, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	if err := e.saveHandoff("k:2", "进度：P2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.withHandoff("k:2", "第一次"); !strings.Contains(got, "进度：P2") {
		t.Fatalf("handoff not injected: %q", got)
	}
	// no ack: simulated Send failure
	got, ack := e.withHandoff("k:2", "重试")
	if !strings.Contains(got, "进度：P2") {
		t.Fatalf("handoff lost after failed send: %q", got)
	}
	ack()
	if again, _ := e.withHandoff("k:2", "之后"); again != "之后" {
		t.Fatalf("handoff must be cleared after ack: %q", again)
	}
}

type rotateSession struct {
	*controllableAgentSession
	reply  string
	onSend func()
}

func (s *rotateSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	if s.onSend != nil {
		s.onSend()
	}
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
	if got, _ := e.withHandoff(key, "next"); !strings.Contains(got, "正在做 #836") {
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

// A message queued while the summary is being written must not be dropped by the
// rotation cleanup: rotation is postponed and the queue is left intact (review P1).
func TestRunRotate_PostponedWhenMessageQueuedDuringSummary(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	base := newControllableSession("rot3")
	state := &interactiveState{platform: p, replyCtx: "ctx"}
	sess := &rotateSession{controllableAgentSession: base, reply: "摘要", onSend: func() {
		state.mu.Lock()
		state.pendingMessages = append(state.pendingMessages, queuedMessage{content: "摘要期间到达"})
		state.mu.Unlock()
	}}
	state.agentSession = sess
	e := NewEngine("t", &controllableAgent{nextSession: base}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	key := "test:rot3"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("old-id", "claudecode")
	session.TryLock()

	if e.runRotate(state, session, e.sessions, key, p, "ctx", 0) {
		t.Fatal("must not rotate when a message arrived during the summary turn")
	}
	if session.GetAgentSessionID() != "old-id" {
		t.Fatal("current session must be kept")
	}
	state.mu.Lock()
	n := len(state.pendingMessages)
	state.mu.Unlock()
	if n != 1 {
		t.Fatalf("queued message must stay queued, got %d", n)
	}
	if got, _ := e.withHandoff(key, "next"); got != "next" {
		t.Fatalf("no pending handoff may be left for the old session: %q", got)
	}
}

// Barrier exactly before the commit: a message queued there aborts the rotation and
// stays queued; nothing may answer it with "session reset" (review P1, round 2).
func TestRunRotate_MessageAtCommitBarrierIsKept(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	base := newControllableSession("rot4")
	state := &interactiveState{platform: p, replyCtx: "ctx"}
	state.agentSession = &rotateSession{controllableAgentSession: base, reply: "摘要"}
	e := NewEngine("t", &controllableAgent{nextSession: base}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	key := "test:rot4"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("old-id", "claudecode")
	session.TryLock()

	rotateBeforeCommitHook = func() {
		state.mu.Lock()
		state.pendingMessages = append(state.pendingMessages, queuedMessage{content: "恰在提交前到达"})
		state.mu.Unlock()
	}
	defer func() { rotateBeforeCommitHook = nil }()

	if e.runRotate(state, session, e.sessions, key, p, "ctx", 0) {
		t.Fatal("must not rotate when a message arrives right before commit")
	}
	state.mu.Lock()
	n, alive := len(state.pendingMessages), state.agentSession != nil
	state.mu.Unlock()
	if n != 1 || !alive || session.GetAgentSessionID() != "old-id" {
		t.Fatalf("rotation must be a no-op: queued=%d alive=%v id=%q", n, alive, session.GetAgentSessionID())
	}
	e.interactiveMu.Lock()
	cur := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if cur != state {
		t.Fatal("state must stay in the map")
	}
	if got, _ := e.withHandoff(key, "next"); got != "next" {
		t.Fatalf("no pending handoff may remain: %q", got)
	}
}

// After commit the old state is gone and the session is free, so a later message
// cannot be queued on the dying state; it takes the normal path instead.
func TestRunRotate_AfterCommitNoQueueOnOldState(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	base := newControllableSession("rot5")
	state := &interactiveState{platform: p, replyCtx: "ctx"}
	state.agentSession = &rotateSession{controllableAgentSession: base, reply: "摘要"}
	e := NewEngine("t", &controllableAgent{nextSession: base}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	key := "test:rot5"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	_, _ = session.TryLock()

	if !e.runRotate(state, session, e.sessions, key, p, "ctx", 0) {
		t.Fatal("expected rotation")
	}
	e.interactiveMu.Lock()
	_, still := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if still {
		t.Fatal("old state must be removed atomically at commit")
	}
	if e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "x"}, key) {
		t.Fatal("no message may be queued on the rotated-out state")
	}
	if _, ok := session.TryLock(); !ok {
		t.Fatal("session must be unlocked at commit so new messages are processed normally")
	}
}
