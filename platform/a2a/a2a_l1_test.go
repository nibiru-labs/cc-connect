package a2a

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

type l1Client struct {
	mu      sync.Mutex
	pending []Inbound
	sent    []l1Sent
	acked   []string
	sendErr error
}

type l1Sent struct{ replyTo, key, text string }

func (c *l1Client) ReadPending(context.Context) ([]Inbound, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Inbound(nil), c.pending...), nil
}
func (c *l1Client) SendReply(_ context.Context, r, k, t string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, l1Sent{r, k, t})
	return nil
}
func (c *l1Client) Ack(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acked = append(c.acked, id)
	return nil
}

type l1Acker struct {
	mu                sync.Mutex
	tracked           map[string][]string
	succeeded, failed int
}

func (a *l1Acker) Track(k string, ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tracked == nil {
		a.tracked = map[string][]string{}
	}
	a.tracked[k] = append(a.tracked[k], ids...)
}
func (a *l1Acker) TurnSucceeded(string)     { a.mu.Lock(); a.succeeded++; a.mu.Unlock() }
func (a *l1Acker) TurnFailed(string, error) { a.mu.Lock(); a.failed++; a.mu.Unlock() }

func TestDispatchMapsAndDedups(t *testing.T) {
	c := &l1Client{pending: []Inbound{{MessageID: "m1", ConversationID: "c1", SenderID: "dot", Text: "hi"}}}
	a := &l1Acker{}
	p := newPlatform(c, a, nil)
	var got []*core.Message
	h := func(_ core.Platform, m *core.Message) { got = append(got, m) }
	p.pollOnce(context.Background(), h)
	p.pollOnce(context.Background(), h) // still pending, in flight
	if len(got) != 1 {
		t.Fatalf("want 1 dispatch, got %d", len(got))
	}
	m := got[0]
	if m.SessionKey != "a2a:c1" || m.Platform != "a2a" || m.UserID != "dot" || m.Content != "hi" || m.MessageID != "m1" {
		t.Fatalf("bad mapping: %+v", m)
	}
	if a.tracked["a2a:c1"][0] != "m1" {
		t.Fatalf("not tracked: %v", a.tracked)
	}
}

func TestNonRosterAckedAndDropped(t *testing.T) {
	c := &l1Client{pending: []Inbound{{MessageID: "m1", ConversationID: "c1", SenderID: "stranger", Text: "x"}}}
	p := newPlatform(c, &l1Acker{}, nil)
	n := 0
	p.pollOnce(context.Background(), func(core.Platform, *core.Message) { n++ })
	if n != 0 || len(c.acked) != 1 || c.acked[0] != "m1" {
		t.Fatalf("n=%d acked=%v", n, c.acked)
	}
}

func TestPongHookConsumes(t *testing.T) {
	c := &l1Client{pending: []Inbound{{MessageID: "m1", ConversationID: "c1", SenderID: "claude-cloud", Text: "ping"}}}
	p := newPlatform(c, &l1Acker{}, nil)
	p.pong = func(context.Context, Inbound) bool { return true }
	n := 0
	p.pollOnce(context.Background(), func(core.Platform, *core.Message) { n++ })
	if n != 0 {
		t.Fatal("pong must not reach the model")
	}
}

func TestReplySuccessAndFailure(t *testing.T) {
	c := &l1Client{}
	a := &l1Acker{}
	p := newPlatform(c, a, nil)
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"m1"}}
	if err := p.Reply(context.Background(), rc, "ok"); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 1 || c.sent[0].replyTo != "m1" || c.sent[0].key != "m1-1" || c.sent[0].text != "ok" {
		t.Fatalf("sent=%+v", c.sent)
	}
	// a second reply in the same turn must not reuse the idempotency key
	_ = p.Send(context.Background(), rc, "more")
	if c.sent[1].key == c.sent[0].key {
		t.Fatal("idempotency key reused")
	}
	c.sendErr = errors.New("boom")
	rc2 := &replyContext{ConversationID: "c2", MessageIDs: []string{"m2"}}
	if err := p.Reply(context.Background(), rc2, "x"); err == nil {
		t.Fatal("want send error")
	}
}

func TestChunking(t *testing.T) {
	text := strings.Repeat("你好世界", 500) // 6000 bytes
	text += strings.Repeat("a", 100)
	c := &l1Client{}
	p := newPlatform(c, &l1Acker{}, map[string]any{"max_chunk": 1000})
	if err := p.Reply(context.Background(), &replyContext{ConversationID: "c", MessageIDs: []string{"m"}}, text); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) < 7 {
		t.Fatalf("too few chunks: %d", len(c.sent))
	}
	var sb strings.Builder
	for i, s := range c.sent {
		if len(s.text) > 1000 || !utf8.ValidString(s.text) {
			t.Fatalf("chunk %d bad (len %d)", i, len(s.text))
		}
		if !strings.HasPrefix(s.text, "[") || !strings.Contains(s.text, "/") || !strings.Contains(s.text, " sha256:") {
			t.Fatalf("no prefix: %q", s.text[:20])
		}
		if s.key != "m-"+itoa(i+1) {
			t.Fatalf("key %q", s.key)
		}
		sb.WriteString(s.text[strings.Index(s.text, "] ")+2:])
	}
	if sb.String() != text {
		t.Fatal("reassembled text differs")
	}
	if got := splitChunks("short", 1000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text must not be prefixed: %v", got)
	}
}

func itoa(i int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i))) }

func TestStopEndsPolling(t *testing.T) {
	p := newPlatform(&l1Client{}, &l1Acker{}, map[string]any{"poll_interval": "10ms"})
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestParseInbound(t *testing.T) {
	for _, in := range []string{
		`[{"message_id":"a","conversation_id":"c","sender_id":"dot","in_reply_to":"","text":"t"}]`,
		`{"messages":[{"message_id":"a","conversation_id":"c","sender_id":"dot","text":"t"}]}`,
	} {
		got, err := parseInbound([]byte(in))
		if err != nil || len(got) != 1 || got[0].MessageID != "a" || got[0].Text != "t" {
			t.Fatalf("%s -> %v %v", in, got, err)
		}
	}
	if got, err := parseInbound([]byte("  ")); err != nil || got != nil {
		t.Fatal("empty output should be no messages")
	}
}

func TestNewOptions(t *testing.T) {
	pl, err := New(map[string]any{"a2a_bin": "/x/a2a", "allow_from": "dot", "poll_interval": 2, "max_chunk": 100})
	if err != nil {
		t.Fatal(err)
	}
	p := pl.(*Platform)
	if p.interval != 2*time.Second || p.maxChunk != 100 || !p.allow["dot"] || p.allow["grok"] {
		t.Fatalf("%+v", p)
	}
	d := newPlatform(nil, nil, nil)
	if d.interval != 5*time.Second || d.maxChunk != 6000 || !d.allow["claude-cloud"] || !d.allow["hermes"] {
		t.Fatal("bad defaults")
	}
}

// Reply/Send only deliver; the turn completes (dedup + ack) on FinishTurn(ok).
func TestSendDoesNotCompleteTurnFinishTurnDoes(t *testing.T) {
	c := &l1Client{pending: []Inbound{{MessageID: "m1", ConversationID: "c1", SenderID: "dot", Text: "hi"}}}
	a := &l1Acker{}
	p := newPlatform(c, a, nil)
	n := 0
	h := func(core.Platform, *core.Message) { n++ }
	p.pollOnce(context.Background(), h)
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"m1"}}
	if err := p.Send(context.Background(), rc, "receipt"); err != nil {
		t.Fatal(err)
	}
	if err := p.Reply(context.Background(), rc, "final"); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 2 || a.succeeded != 0 || a.failed != 0 {
		t.Fatalf("deliveries must not touch the acker: sent=%d succ=%d failed=%d", len(c.sent), a.succeeded, a.failed)
	}
	p.pollOnce(context.Background(), h)
	if n != 1 {
		t.Fatalf("in-flight turn re-dispatched, dispatches=%d", n)
	}
	p.FinishTurn(context.Background(), rc, true)
	if a.succeeded != 1 || a.failed != 0 {
		t.Fatalf("FinishTurn(ok) must succeed once: succ=%d failed=%d", a.succeeded, a.failed)
	}
	p.pollOnce(context.Background(), h)
	if n != 1 {
		t.Fatalf("completed turn re-dispatched, dispatches=%d", n)
	}
}

func TestFinishTurnFailedReleasesForRedelivery(t *testing.T) {
	c := &l1Client{pending: []Inbound{{MessageID: "m1", ConversationID: "c1", SenderID: "dot", Text: "hi"}}}
	a := &l1Acker{}
	p := newPlatform(c, a, nil)
	n := 0
	h := func(core.Platform, *core.Message) { n++ }
	p.pollOnce(context.Background(), h)
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"m1"}}
	p.FinishTurn(context.Background(), rc, false)
	if a.failed != 1 || a.succeeded != 0 {
		t.Fatalf("succ=%d failed=%d", a.succeeded, a.failed)
	}
	p.pollOnce(context.Background(), h)
	if n != 2 {
		t.Fatalf("failed turn should be redelivered, dispatches=%d", n)
	}
}

// A failed delivery is just an error; only FinishTurn decides the turn outcome.
func TestSendFailureDoesNotTouchAcker(t *testing.T) {
	c := &l1Client{sendErr: errors.New("boom")}
	a := &l1Acker{}
	p := newPlatform(c, a, nil)
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"m1"}}
	if err := p.Send(context.Background(), rc, "receipt"); err == nil {
		t.Fatal("want send error")
	}
	if a.failed != 0 || a.succeeded != 0 {
		t.Fatalf("succ=%d failed=%d", a.succeeded, a.failed)
	}
}

var _ core.TurnFinisher = (*Platform)(nil)
