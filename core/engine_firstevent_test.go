package core

import (
	"strings"
	"testing"
	"time"
)

// nibiru#836: an agent that never emits any event must not hold the turn forever.
func TestFirstEventTimeout_FailsAndReleases(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("first-evt")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetFirstEventTimeout(100 * time.Millisecond)

	key := "test:first-evt"
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "", time.Now(), nil, nil, nil, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not end without any agent event")
	}
	found := false
	for _, s := range p.getSent() {
		if strings.Contains(s, "did not respond") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected failure notice, got %v", p.getSent())
	}
	e.interactiveMu.Lock()
	_, still := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if still {
		t.Fatal("interactive state should be cleaned up so the queue is released")
	}
}

func TestFirstEventTimeout_NotTriggeredAfterEvent(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("first-evt-ok")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetFirstEventTimeout(150 * time.Millisecond)

	key := "test:first-evt-ok"
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "", time.Now(), nil, nil, nil, 0)
		close(done)
	}()
	sess.events <- Event{Type: EventText, Content: "hi"}
	time.Sleep(400 * time.Millisecond) // longer than the first-event window
	sess.events <- Event{Type: EventResult, Content: "done", Done: true}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not complete")
	}
	for _, s := range p.getSent() {
		if strings.Contains(s, "did not respond") {
			t.Fatalf("must not time out after the first event: %v", p.getSent())
		}
	}
}
