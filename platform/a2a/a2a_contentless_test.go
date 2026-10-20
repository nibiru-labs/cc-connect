package a2a

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// nibiru#1431: a contentless inbound used to stay in flight forever. The
// platform marks the message in flight and the Acker tracks it for the
// session, but the engine returned early without settling the turn, so
// neither the in-flight mark nor the tracking was ever released: the same
// message id could never be admitted again.
//
// These tests cover the platform half of the fix: once the turn is settled
// (core/engine.go calls FinishTurn for a contentless inbound), the message is
// acked, the in-flight mark is gone and the id is admissible again.

// noopAgent is a model that never produces a turn; the contentless inbound
// must never reach it.
type noopAgent struct{}

func (a *noopAgent) Name() string { return "noop" }
func (a *noopAgent) StartSession(_ context.Context, _ string) (core.AgentSession, error) {
	return &noopSession{}, nil
}
func (a *noopAgent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (a *noopAgent) Stop() error                                                     { return nil }

type noopSession struct{}

func (s *noopSession) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	return nil
}
func (s *noopSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *noopSession) Events() <-chan core.Event                             { return make(chan core.Event) }
func (s *noopSession) CurrentSessionID() string                              { return "noop" }
func (s *noopSession) Alive() bool                                           { return true }
func (s *noopSession) Close() error                                          { return nil }

// The real engine must settle a contentless inbound, so the platform can ack
// it and release its in-flight mark.
func TestContentlessInboundIsSettledAndAcked(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	ctx := context.Background()

	e := core.NewEngine("test", &noopAgent{}, []core.Platform{p}, "", core.LangEnglish)
	defer func() { _ = e.Stop() }()

	p.dispatch(ctx, e.ReceiveMessage, Inbound{
		MessageID:      "am-blank",
		ConversationID: "ac-conv",
		SenderID:       "dot",
		Text:           "   ",
	})

	acked := c.ackedCopy()
	if len(acked) != 1 || acked[0] != "am-blank" {
		t.Fatalf("acked = %#v, want [am-blank]: a contentless inbound must be settled", acked)
	}
	p.mu.Lock()
	inflight := len(p.inflight)
	p.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d, want 0: a contentless inbound must not stay in flight", inflight)
	}
	if !p.acker.Admit("am-blank") {
		t.Fatalf("Admit returned false after the settlement: the id must be admissible again")
	}
}

// While the turn is running the message is tracked, and the tracking is
// released as soon as the turn is settled.
func TestContentlessInboundTrackedUntilSettled(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	r := &recorder{}
	ctx := context.Background()

	p.dispatch(ctx, r.handle, Inbound{
		MessageID:      "am-blank",
		ConversationID: "ac-conv",
		SenderID:       "dot",
		Text:           "   ",
	})

	msgs := r.all()
	if len(msgs) != 1 {
		t.Fatalf("dispatched messages = %d, want 1", len(msgs))
	}
	if p.acker.Admit("am-blank") {
		t.Fatalf("Admit returned true while the message is still tracked")
	}
	p.mu.Lock()
	inflight := len(p.inflight)
	p.mu.Unlock()
	if inflight != 1 {
		t.Fatalf("inflight = %d, want 1 before the settlement", inflight)
	}

	p.FinishTurn(ctx, msgs[0].ReplyCtx, true)

	acked := c.ackedCopy()
	if len(acked) != 1 || acked[0] != "am-blank" {
		t.Fatalf("acked = %#v, want [am-blank]", acked)
	}
	p.mu.Lock()
	inflight = len(p.inflight)
	p.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d, want 0 after the settlement", inflight)
	}
	if !p.acker.Admit("am-blank") {
		t.Fatalf("Admit returned false after the settlement: the id must be admissible again")
	}
}
