package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rotationRacePlatform struct {
	*stubPlatformEngine
	recallEntered chan struct{}
	recallRelease chan struct{}
	recallOnce    sync.Once
}

func (p *rotationRacePlatform) IsMessageRecalled(ctx context.Context, _ any) (bool, error) {
	p.recallOnce.Do(func() { close(p.recallEntered) })
	select {
	case <-p.recallRelease:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// A real Engine.ReceiveMessage can observe the old session as busy, then lose
// the queue target when rotation removes interactiveState and unlocks that
// session. It must claim the released lock and start a turn for the message.
func TestReceiveMessage_RotationCommitDoesNotDropInboundMessage(t *testing.T) {
	p := &rotationRacePlatform{
		stubPlatformEngine: &stubPlatformEngine{n: "test"},
		recallEntered:      make(chan struct{}),
		recallRelease:      make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseRecall := func() { releaseOnce.Do(func() { close(p.recallRelease) }) }
	t.Cleanup(releaseRecall)

	nextSession := newControllableSession("new-session")
	started := make(chan struct{}, 1)
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		started <- struct{}{}
		return nextSession, nil
	}}
	e := NewEngine("rotation-race", agent, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetDataDir(t.TempDir())
	e.SetUnifiedSession(true)

	key := UnifiedSessionKey("rotation-race")
	oldBase := newControllableSession("old-session")
	oldSession := &rotateSession{controllableAgentSession: oldBase, reply: "handoff summary"}
	state := &interactiveState{
		agentSession:     oldSession,
		platform:         p,
		replyCtx:         "old-reply",
		currentMessageID: "current-turn",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("old-session", "controllable")
	lockGen, locked := session.TryLock()
	if !locked {
		t.Fatal("failed to lock the session for rotation")
	}

	var accepted atomic.Int32
	received := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{
			SessionKey: "test:source-chat",
			Platform:   "test",
			UserID:     "sender",
			MessageID:  "incoming",
			Content:    "continue after rotation",
			ReplyCtx:   "incoming-reply",
			OnAccepted: func() { accepted.Add(1) },
		})
		close(received)
	}()

	select {
	case <-p.recallEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("inbound message did not reach the barrier after its busy-lock attempt")
	}

	if !e.runRotate(state, session, e.sessions, key, p, "old-reply", lockGen) {
		t.Fatal("rotation did not commit at the barrier")
	}
	releaseRecall()

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("inbound message did not leave ReceiveMessage")
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("accepted inbound message %d times, want exactly once", got)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted inbound message did not start the replacement session")
	}
}
