package core

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rotationRaceMessageSession struct {
	*controllableAgentSession
	sent chan string
}

func (s *rotationRaceMessageSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.sent <- prompt
	s.events <- Event{Type: EventResult, Content: "handled", Done: true}
	return nil
}

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

func TestReceiveMessage_RotationCommitPreservesContendingInboundMessages(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	oldBase := newControllableSession("old-session")
	oldSession := &rotateSession{controllableAgentSession: oldBase, reply: "handoff summary"}
	newBase := newControllableSession("new-session")
	newSession := &rotationRaceMessageSession{
		controllableAgentSession: newBase,
		sent:                     make(chan string, 4),
	}
	started := make(chan struct{}, 1)
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		started <- struct{}{}
		return newSession, nil
	}}
	e := NewEngine("rotation-race-contended", agent, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetDataDir(t.TempDir())
	e.SetUnifiedSession(true)
	e.SetStaleLockBreakAfter(10 * time.Minute)

	key := UnifiedSessionKey("rotation-race-contended")
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

	barrierEntered := make(chan struct{}, 2)
	releaseInbound := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseInbound) }) }
	receiveAfterBusyTryLockHook = func() {
		barrierEntered <- struct{}{}
		<-releaseInbound
	}
	t.Cleanup(func() { receiveAfterBusyTryLockHook = nil })

	var accepted atomic.Int32
	receiveDone := make(chan struct{}, 2)
	received := 0
	t.Cleanup(func() {
		release()
		for received < 2 {
			select {
			case <-receiveDone:
				received++
			case <-time.After(3 * time.Second):
				return
			}
		}
	})
	for _, message := range []struct {
		id      string
		session string
		content string
	}{
		{id: "incoming-one", session: "test:chat-one", content: "first concurrent message"},
		{id: "incoming-two", session: "test:chat-two", content: "second concurrent message"},
	} {
		message := message
		go func() {
			e.ReceiveMessage(p, &Message{
				SessionKey: message.session,
				Platform:   "test",
				UserID:     "sender",
				MessageID:  message.id,
				Content:    message.content,
				ReplyCtx:   message.id,
				OnAccepted: func() { accepted.Add(1) },
			})
			receiveDone <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-barrierEntered:
		case <-time.After(3 * time.Second):
			t.Fatal("both inbound messages must observe the busy session before rotation commits")
		}
	}

	if !e.runRotate(state, session, e.sessions, key, p, "old-reply", lockGen) {
		t.Fatal("rotation did not commit at the barrier")
	}
	release()
	for range 2 {
		select {
		case <-receiveDone:
			received++
		case <-time.After(3 * time.Second):
			t.Fatal("a contending inbound message did not leave ReceiveMessage")
		}
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted inbound messages %d times, want exactly twice", got)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement agent session did not start")
	}

	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case prompt := <-newSession.sent:
			got[prompt] = true
		case <-deadline:
			t.Fatalf("replacement session did not receive both messages; prompts=%d", len(got))
		}
	}
	var sawFirst, sawSecond bool
	for prompt := range got {
		sawFirst = sawFirst || strings.Contains(prompt, "first concurrent message")
		sawSecond = sawSecond || strings.Contains(prompt, "second concurrent message")
	}
	if !sawFirst || !sawSecond {
		t.Fatalf("replacement prompts missing inbound content: first=%v second=%v", sawFirst, sawSecond)
	}
	for _, reply := range p.getSent() {
		if strings.Contains(reply, "Previous request still processing") {
			t.Fatalf("a contending message was rejected after rotation: %q", reply)
		}
	}
}
