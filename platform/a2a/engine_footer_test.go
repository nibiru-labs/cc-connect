package a2a

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type footerTestAgent struct {
	response string
	received chan<- string
}

func (a *footerTestAgent) Name() string     { return "a2a-footer-test" }
func (a *footerTestAgent) GetModel() string { return "gpt-6-luna" }
func (a *footerTestAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return &footerTestSession{response: a.response, received: a.received, events: make(chan core.Event, 1)}, nil
}
func (a *footerTestAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *footerTestAgent) Stop() error { return nil }

type footerTestSession struct {
	response string
	received chan<- string
	events   chan core.Event
}

func (s *footerTestSession) Send(prompt string, _ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	if s.received != nil {
		s.received <- prompt
	}
	s.events <- core.Event{Type: core.EventResult, Content: s.response, Done: true}
	return nil
}
func (s *footerTestSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *footerTestSession) Events() <-chan core.Event                             { return s.events }
func (s *footerTestSession) CurrentSessionID() string                              { return "a2a-footer-test-session" }
func (s *footerTestSession) Alive() bool                                           { return true }
func (s *footerTestSession) Close() error                                          { return nil }

func startFooterTestEngine(t *testing.T, c *fakeClient, response string) *core.Engine {
	t.Helper()
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	e := core.NewEngine("a2a-footer-test", &footerTestAgent{response: response}, []core.Platform{p}, "", core.LangEnglish)
	e.SetReplyFooterEnabled(true)
	e.SetShowContextIndicator(true)
	if err := e.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e
}

func startRateLimitTestEngine(t *testing.T, c *fakeClient, received chan<- string) (*core.Engine, *Platform) {
	t.Helper()
	p := newTestPlatform(t, c, newL2Acker(c, 3), nil)
	e := core.NewEngine("a2a-rate-limit-test", &footerTestAgent{response: "accepted", received: received},
		[]core.Platform{p}, "", core.LangEnglish)
	e.SetReplyFooterEnabled(false)
	e.SetRateLimitCfg(core.RateLimitCfg{MaxMessages: 1, Window: time.Minute})
	if err := e.Start(); err != nil {
		t.Fatalf("start rate-limit engine: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e, p
}

func eventuallyAcked(t *testing.T, c *fakeClient, ids ...string) {
	t.Helper()
	eventually(t, fmt.Sprintf("messages %v acknowledged", ids), func() bool {
		acked := c.ackedCopy()
		for _, want := range ids {
			found := false
			for _, got := range acked {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
}

func TestCoreA2AReplyFooterEmptyResultDoesNotPublishAndAcks(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("empty-seed", "empty-conversation", "grok", "please answer")}}
	startFooterTestEngine(t, c, "")
	eventuallyAcked(t, c, "empty-seed")
	time.Sleep(30 * time.Millisecond)
	if sent := c.sentCopy(); len(sent) != 0 {
		t.Fatalf("empty EventResult published with footer enabled: %+v", sent)
	}
}

func TestCoreA2AReplyFooterBareNoReplyDoesNotPublishAndAcks(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("silent-seed", "silent-conversation", "grok", "please answer")}}
	startFooterTestEngine(t, c, "NO_REPLY")
	eventuallyAcked(t, c, "silent-seed")
	if sent := c.sentCopy(); len(sent) != 0 {
		t.Fatalf("bare NO_REPLY was published: %+v", sent)
	}
}

func TestCoreA2AReplyFooterPreservesBodyAndTrailingNoReply(t *testing.T) {
	t.Run("ordinary body", func(t *testing.T) {
		c := &fakeClient{pending: []Inbound{in("body-seed", "body-conversation", "grok", "please answer")}}
		startFooterTestEngine(t, c, "answer")
		eventuallyAcked(t, c, "body-seed")
		eventually(t, "body and footer sent", func() bool { return len(c.sentCopy()) == 1 })
		if got, want := c.sentCopy()[0].Text, "answer\n\n*gpt-6-luna*"; got != want {
			t.Fatalf("sent body/footer = %q, want %q", got, want)
		}
	})

	t.Run("empty-placeholder-prefix-is-real-body", func(t *testing.T) {
		c := &fakeClient{pending: []Inbound{in("placeholder-body-seed", "placeholder-body-conversation", "grok", "please answer")}}
		body := "(空响应)\n\n*这是一条普通正文*"
		startFooterTestEngine(t, c, body)
		eventuallyAcked(t, c, "placeholder-body-seed")
		eventually(t, "placeholder-looking body and footer sent", func() bool { return len(c.sentCopy()) == 1 })
		if got, want := c.sentCopy()[0].Text, body+"\n\n*gpt-6-luna*"; got != want {
			t.Fatalf("sent body/footer = %q, want %q", got, want)
		}
	})

	t.Run("trailing marker", func(t *testing.T) {
		c := &fakeClient{pending: []Inbound{in("marker-seed", "marker-conversation", "grok", "please answer")}}
		startFooterTestEngine(t, c, "answer\nNO_REPLY")
		eventuallyAcked(t, c, "marker-seed")
		eventually(t, "body and footer sent", func() bool { return len(c.sentCopy()) == 1 })
		if got, want := c.sentCopy()[0].Text, "answer\n\n*gpt-6-luna*"; got != want {
			t.Fatalf("sent body/footer = %q, want %q", got, want)
		}
	})
}

func TestCoreA2AReplyFooterTwoPeerSeedsAreAckedWithoutPublishing(t *testing.T) {
	c := &fakeClient{pending: []Inbound{
		in("bobo-seed", "bobo-conversation", "grok", "reply"),
		in("naya-seed", "naya-conversation", "dot", "reply"),
	}}
	startFooterTestEngine(t, c, "")
	eventuallyAcked(t, c, "bobo-seed", "naya-seed")
	time.Sleep(30 * time.Millisecond)
	if sent := c.sentCopy(); len(sent) != 0 {
		t.Fatalf("empty peer turns published bus messages: %+v", sent)
	}
}

func TestCoreA2ARateLimitedSystemResponseIsSuppressedAndSettledOnce(t *testing.T) {
	rateLimitText := core.NewI18n(core.LangEnglish).T(core.MsgRateLimited)
	c := &fakeClient{pending: []Inbound{
		in("accepted-seed", "limited-conversation", "grok", "first user prompt"),
		in("limited-seed", "limited-conversation", "grok", "second user prompt"),
		// A user is allowed to send text identical to the system response.
		in("same-text-seed", "same-text-conversation", "grok", rateLimitText),
	}}
	received := make(chan string, 3)
	_, p := startRateLimitTestEngine(t, c, received)
	eventuallyAcked(t, c, "accepted-seed", "limited-seed", "same-text-seed")

	for _, id := range []string{"accepted-seed", "limited-seed", "same-text-seed"} {
		count := 0
		for _, got := range c.ackedCopy() {
			if got == id {
				count++
			}
		}
		if count != 1 {
			t.Errorf("message %q ack count=%d, want exactly once; all=%v", id, count, c.ackedCopy())
		}
	}

	eventually(t, "both allowed user prompts reached the agent", func() bool {
		return len(received) == 2
	})
	gotInputs := []string{<-received, <-received}
	containsRateLimitPrompt := false
	containsFirstPrompt := false
	for _, input := range gotInputs {
		if input == rateLimitText {
			containsRateLimitPrompt = true
		}
		if input == "first user prompt" {
			containsFirstPrompt = true
		}
	}
	if !containsRateLimitPrompt {
		t.Fatalf("user prompt equal to system response was filtered: inputs=%q", gotInputs)
	}
	if !containsFirstPrompt {
		t.Fatalf("first allowed user prompt missing: inputs=%q", gotInputs)
	}

	eventually(t, "only two agent replies published", func() bool { return len(c.sentCopy()) >= 2 })
	sent := c.sentCopy()
	if len(sent) != 2 {
		t.Fatalf("sent %d A2A replies, want only the two completed user turns: %+v", len(sent), sent)
	}
	for _, reply := range sent {
		if reply.Text != "accepted" {
			t.Fatalf("unexpected system reply was published to A2A: %+v", sent)
		}
	}
	if got := p.empty.count("limited-conversation"); got != 0 {
		t.Fatalf("suppressed rate-limit response changed empty-reply breaker count: %d", got)
	}
	p.empty.mu.Lock()
	_, pendingEmptyTurn := p.empty.inboundEmpty["limited-seed"]
	p.empty.mu.Unlock()
	if pendingEmptyTurn {
		t.Fatal("suppressed system turn leaked into empty-breaker pending state")
	}
	p.mu.Lock()
	inflight := p.inflight["limited-seed"]
	_, completed := p.done["limited-seed"]
	p.mu.Unlock()
	if inflight || !completed {
		t.Fatalf("suppressed inbound release state inflight=%t completed=%t", inflight, completed)
	}
}
