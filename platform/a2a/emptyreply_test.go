package a2a

import (
	"context"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// nibiru#1346: bobo and naya kept answering each other's empty replies
// ("(空响应)" <-> "(空响应)") for hundreds of turns in one conversation.
//
// This file holds the behaviour tests that only use APIs that already existed
// before the fix, so the same file reproduces the defect on the pre-fix code.
//
//  1. an empty / placeholder / NO_REPLY reply is never delivered, but the turn
//     is still settled so the inbound message is acked;
//  2. after a few consecutive empty exchanges the platform stops waking the
//     model for that conversation, so the loop cannot continue.
//
// scriptedHandler emulates one engine turn: it replies with a fixed text and
// then settles the turn.
type scriptedHandler struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (h *scriptedHandler) handle(p core.Platform, m *core.Message) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	_ = p.Reply(context.Background(), m.ReplyCtx, h.reply)
	if finisher, ok := p.(core.TurnFinisher); ok {
		finisher.FinishTurn(context.Background(), m.ReplyCtx, true)
	}
}

func (h *scriptedHandler) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func inbound(id, conversation, text string) Inbound {
	return Inbound{MessageID: id, ConversationID: conversation, SenderID: "dot", Text: text}
}

// An empty reply is dropped, but the inbound message is still acked.
func TestEmptyReplyIsNotDeliveredButTurnIsSettled(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	h := &scriptedHandler{reply: "(空响应)"}

	p.dispatch(context.Background(), h.handle, inbound("am-1", "ac-conv", "(空响应)"))

	if sent := c.sentCopy(); len(sent) != 0 {
		t.Fatalf("sent = %#v, want no delivery for an empty reply", sent)
	}
	acked := c.ackedCopy()
	if len(acked) != 1 || acked[0] != "am-1" {
		t.Fatalf("acked = %#v, want [am-1]", acked)
	}
}

// A real reply is still delivered.
func TestRealReplyIsStillDelivered(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	h := &scriptedHandler{reply: "a real answer"}

	p.dispatch(context.Background(), h.handle, inbound("am-1", "ac-conv", "ping"))

	sent := c.sentCopy()
	if len(sent) != 1 || sent[0].Text != "a real answer" {
		t.Fatalf("sent = %#v, want one real reply", sent)
	}
}

// The empty-reply loop stops: after the empty-exchange budget is used up, an
// empty inbound is acked without waking the model at all.
func TestEmptyReplyLoopStops(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	h := &scriptedHandler{reply: "(空响应)"}

	for i := 1; i <= 4; i++ {
		p.dispatch(context.Background(), h.handle, inbound("am-"+string(rune('0'+i)), "ac-conv", "(空响应)"))
	}
	if h.callCount() > 3 {
		t.Fatalf("handler calls = %d, want at most 3: the loop must stop", h.callCount())
	}
	if sent := c.sentCopy(); len(sent) != 0 {
		t.Fatalf("sent = %#v, want no delivery at all", sent)
	}
	// Every inbound message is acked, including the one dropped by the breaker.
	acked := c.ackedCopy()
	if len(acked) != 4 {
		t.Fatalf("acked = %#v, want 4 acks", acked)
	}
}

// A conversation that never exchanges empty content is never affected.
func TestNormalConversationIsUnaffected(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	h := &scriptedHandler{reply: "answer"}

	for i := 1; i <= 5; i++ {
		p.dispatch(context.Background(), h.handle, inbound("am-"+string(rune('0'+i)), "ac-conv", "hello"))
	}
	if h.callCount() != 5 {
		t.Fatalf("handler calls = %d, want 5", h.callCount())
	}
	if len(c.sentCopy()) != 5 {
		t.Fatalf("sent = %d, want 5", len(c.sentCopy()))
	}
}
