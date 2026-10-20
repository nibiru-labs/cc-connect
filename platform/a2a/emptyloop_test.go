package a2a

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// nibiru#1346
func TestIsEmptyReply(t *testing.T) {
	for _, s := range []string{"", "  \n", "(空响应)", " (空响应)\n", "(empty response)", "(空回應)", "（空のレスポンス）", "(respuesta vacía)", "NO_REPLY", " no_reply \n"} {
		if !isEmptyReply(s) {
			t.Errorf("isEmptyReply(%q)=false, want true", s)
		}
	}
	for _, s := range []string{"ok", "(空响应) 但有内容", "NO_REPLY please", "收到",
		"说明：(空响应)\n\n*gpt-6-luna · ctx 9%*", "(空响应)\n\n这是正文，不是 core footer"} {
		if isEmptyReply(s) {
			t.Errorf("isEmptyReply(%q)=true, want false", s)
		}
	}
}

func TestFooterDecoratedPlaceholderIsAmbiguousWithoutMetadata(t *testing.T) {
	for _, s := range []string{
		"(空响应)\n\n*gpt-6-luna · ctx 9%*",
		"(空响应)\n\n*这是一条普通正文*",
	} {
		if isEmptyReply(s) {
			t.Errorf("isEmptyReply(%q)=true without trusted footer metadata", s)
		}
	}
}

// Empty / placeholder / NO_REPLY replies are never sent, yet the turn still settles.
func TestEmptyReplyNotSentButTurnAcked(t *testing.T) {
	for _, body := range []string{"", "(空响应)", "(empty response)", "NO_REPLY"} {
		c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
		ak := newFakeAcker()
		p, r := startP(t, c, ak, nil)
		rc := replyCtxOf(t, r)
		if err := p.Reply(context.Background(), rc, body); err != nil {
			t.Fatalf("%q: Reply err %v", body, err)
		}
		p.FinishTurn(context.Background(), rc, true)
		if sent := c.sentCopy(); len(sent) != 0 {
			t.Fatalf("%q: placeholder was sent: %+v", body, sent)
		}
		ak.mu.Lock()
		okN := len(ak.ok)
		ak.mu.Unlock()
		if okN != 1 {
			t.Fatalf("%q: turn not settled, ok=%d", body, okN)
		}
		_ = p.Stop()
	}
}

// After 3 consecutive empty exchanges the breaker acks further empty inbound
// messages without dispatching them to the model; real content resets it.
func TestEmptyExchangeBreaker(t *testing.T) {
	c := &fakeClient{}
	acker := newFakeAcker()
	p := newTestPlatform(t, c, acker, nil)
	r := &recorder{}
	ctx := context.Background()

	exchange := func(id, text, reply string) bool {
		before := len(r.all())
		p.dispatch(ctx, r.handle, in(id, "loop", "grok", text))
		if len(r.all()) == before {
			return false // dropped by breaker
		}
		rc := r.all()[len(r.all())-1].ReplyCtx
		_ = p.Reply(ctx, rc, reply)
		p.FinishTurn(ctx, rc, true)
		return true
	}

	for i := 1; i <= DefaultEmptyExchangeLimit; i++ {
		if !exchange(fmt.Sprintf("e%d", i), "(空响应)", "(空响应)") {
			t.Fatalf("exchange %d dropped before limit", i)
		}
	}
	if got := p.empty.count("loop"); got != DefaultEmptyExchangeLimit {
		t.Fatalf("count=%d", got)
	}
	if exchange("e4", "(空响应)", "") {
		t.Fatal("breaker did not stop the 4th empty exchange")
	}
	acker.mu.Lock()
	settled := append([]string(nil), acker.ok...)
	acker.mu.Unlock()
	settledE4 := 0
	for _, id := range settled {
		if id == "e4" {
			settledE4++
		}
	}
	if settledE4 != 1 {
		t.Fatalf("dropped message must be settled through the Acker, settled=%v", settled)
	}
	if len(c.sentCopy()) != 0 {
		t.Fatalf("nothing should be sent: %+v", c.sentCopy())
	}
	// Other conversations are unaffected.
	p.dispatch(ctx, r.handle, in("o1", "other", "grok", "(空响应)"))
	if len(r.all()) != 4 {
		t.Fatalf("other conversation was blocked; dispatched=%d", len(r.all()))
	}
	// A real message still gets through and a real exchange resets the breaker.
	if !exchange("real", "请看 #1346", "好的，已处理") {
		t.Fatal("real message dropped")
	}
	if got := p.empty.count("loop"); got != 0 {
		t.Fatalf("breaker not reset, count=%d", got)
	}
	if !exchange("e5", "(空响应)", "") {
		t.Fatal("empty message after reset should be dispatched once")
	}
}

// End-to-end through the poll loop: two placeholder messages in a row only
// cost model turns until the limit, never a send.
func TestEmptyLoopPollingStops(t *testing.T) {
	c := &fakeClient{}
	p, r := startP(t, c, newL2Acker(c, 3), map[string]any{"empty_reply_limit": 2})
	ctx := context.Background()
	handled := map[string]bool{}
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("p%d", i)
		c.mu.Lock()
		c.pending = append(c.pending, in(id, "ac-loop", "grok", "(空响应)"))
		c.mu.Unlock()
		eventually(t, id+" settled", func() bool {
			for _, m := range r.all() {
				if m.MessageID == id && !handled[id] {
					handled[id] = true
					_ = p.Reply(ctx, m.ReplyCtx, "(空响应)")
					p.FinishTurn(ctx, m.ReplyCtx, true)
				}
			}
			for _, a := range c.ackedCopy() {
				if a == id {
					return true
				}
			}
			return false
		})
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(r.all()); n != 2 {
		t.Fatalf("model turns=%d, want 2 (limit)", n)
	}
	if len(c.sentCopy()) != 0 {
		t.Fatalf("placeholder sent: %+v", c.sentCopy())
	}
}
