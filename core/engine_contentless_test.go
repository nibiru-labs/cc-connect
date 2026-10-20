package core

import (
	"context"
	"sync/atomic"
	"testing"
)

// nibiru#1431: a contentless inbound (empty or whitespace-only) used to return
// early in handleMessage without settling the turn. Queue-backed platforms
// (A2A) then kept the message tracked forever: the platform's in-flight mark
// and the Acker tracking both survived, and the same message id could never be
// admitted again.
//
// The fix settles the turn through TurnFinisher, so the inbound message is
// acked and released without ever starting a model turn.

// countingAgent records whether a model session was started.
type countingAgent struct {
	stubAgent
	starts atomic.Int32
}

func (a *countingAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.starts.Add(1)
	return &stubAgentSession{}, nil
}

func TestHandleMessageContentlessInboundSettlesTurn(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"whitespace", "\n\t  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubTurnFinisherPlatform{stubPlatformEngine: stubPlatformEngine{n: "a2a"}}
			agent := &countingAgent{}
			e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

			e.ReceiveMessage(p, &Message{
				SessionKey: "a2a:conv",
				Platform:   "a2a",
				MessageID:  "am-blank",
				UserID:     "dot",
				Content:    tc.content,
			})

			fins := p.finishes()
			if len(fins) != 1 {
				t.Fatalf("finishes = %#v, want exactly one settled turn", fins)
			}
			if !fins[0].ok {
				t.Fatalf("finish ok = false, want true: a contentless inbound is terminal, not a failure")
			}
			if agent.starts.Load() != 0 {
				t.Fatalf("agent sessions started = %d, want 0: a contentless inbound must not reach the model", agent.starts.Load())
			}
			if sent := p.getSent(); len(sent) != 0 {
				t.Fatalf("sent = %#v, want no reply for a contentless inbound", sent)
			}
		})
	}
}

// A platform that does not implement TurnFinisher must not break on a
// contentless inbound: the turn is simply dropped as before.
func TestHandleMessageContentlessInboundWithoutTurnFinisher(t *testing.T) {
	p := &stubPlatformEngine{n: "a2a"}
	agent := &countingAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.ReceiveMessage(p, &Message{
		SessionKey: "a2a:conv",
		Platform:   "a2a",
		MessageID:  "am-blank",
		Content:    "   ",
	})

	if agent.starts.Load() != 0 {
		t.Fatalf("agent sessions started = %d, want 0", agent.starts.Load())
	}
}
