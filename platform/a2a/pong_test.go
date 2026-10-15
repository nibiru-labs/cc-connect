package a2a

import (
	"context"
	"errors"
	"testing"
)

const testPing = `{"type":"nibiru.ping.v1","request_id":"0123456789abcdef","from":"claude-cloud","sent_at":1760000000}`

func TestPingRequestID(t *testing.T) {
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{testPing, "0123456789abcdef", true},
		{"  " + testPing + "\n", "0123456789abcdef", true},
		{`{"type":"nibiru.ping.v1","request_id":"XYZ"}`, "", false},
		{`{"type":"nibiru.ping.v1","request_id":"0123456789ABCDEF"}`, "", false},
		{`{"type":"nibiru.pong.v1","request_id":"0123456789abcdef"}`, "", false},
		{`not json`, "", false},
		{`{"type":1}`, "", false},
		{``, "", false},
	}
	for _, c := range cases {
		got, ok := PingRequestID(c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("PingRequestID(%q) = %q,%v", c.text, got, ok)
		}
	}
}

func TestPongBody(t *testing.T) {
	if got := PongBody("0123456789abcdef"); got != `{"type":"nibiru.pong.v1","request_id":"0123456789abcdef"}` {
		t.Fatal(got)
	}
}

func TestPongHandlerAnswersClaudeCloud(t *testing.T) {
	c := &ackFakeClient{}
	p := NewPongHandler(c, 5)
	handled := p.Handle(context.Background(), Inbound{MessageID: "m1", SenderID: "claude-cloud", Text: testPing})
	if !handled {
		t.Fatal("ping must be handled")
	}
	if len(c.replies) != 1 || c.replies[0].replyTo != "m1" || c.replies[0].key != "pong-0123456789abcdef" ||
		c.replies[0].text != PongBody("0123456789abcdef") {
		t.Fatalf("replies = %+v", c.replies)
	}
	if got := c.ackedIDs(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("acked %v", got)
	}
}

func TestPongHandlerIgnoresOtherSenders(t *testing.T) {
	c := &ackFakeClient{}
	p := NewPongHandler(c, 5)
	if !p.Handle(context.Background(), Inbound{MessageID: "m1", SenderID: "grok", Text: testPing}) {
		t.Fatal("still handled (ack only)")
	}
	if len(c.replies) != 0 {
		t.Fatal("must not reply to non-claude-cloud ping")
	}
	if len(c.ackedIDs()) != 1 {
		t.Fatal("must ack")
	}
}

func TestPongHandlerPassesThroughNormalMessages(t *testing.T) {
	c := &ackFakeClient{}
	p := NewPongHandler(c, 5)
	if p.Handle(context.Background(), Inbound{MessageID: "m1", SenderID: "claude-cloud", Text: "hello"}) {
		t.Fatal("normal message must go to the model")
	}
	if len(c.ackedIDs()) != 0 || len(c.replies) != 0 {
		t.Fatal("no side effects")
	}
}

func TestPongHandlerRetriesThenAcks(t *testing.T) {
	c := &ackFakeClient{sendErr: errors.New("down")}
	p := NewPongHandler(c, 3)
	in := Inbound{MessageID: "m1", SenderID: "claude-cloud", Text: testPing}
	for i := 0; i < 2; i++ {
		if !p.Handle(context.Background(), in) {
			t.Fatal("handled")
		}
		if len(c.ackedIDs()) != 0 {
			t.Fatalf("round %d: must not ack before attempts exhausted", i)
		}
	}
	p.Handle(context.Background(), in) // 3rd failure → give up and ack
	if len(c.ackedIDs()) != 1 {
		t.Fatal("must ack after max attempts")
	}
}
