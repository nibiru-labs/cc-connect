package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

type settlementObservation struct {
	id         string
	stateReady bool
}

type footerIntegrationAcker struct {
	mu            sync.Mutex
	client        Client
	succeeded     []string
	failed        []string
	observations  []settlementObservation
	statePath     string
	readyRosterID string
}

func (a *footerIntegrationAcker) Admit(string) bool      { return true }
func (a *footerIntegrationAcker) Track(string, []string) {}
func (a *footerIntegrationAcker) TurnSucceededIDs(ids []string) {
	for _, id := range ids {
		stateReady := true
		if id == a.readyRosterID {
			data, err := os.ReadFile(a.statePath)
			var state rosterTransferDiskState
			stateReady = err == nil && json.Unmarshal(data, &state) == nil &&
				state.HighWater != nil && state.HighWater.State == "delivered"
		}
		a.mu.Lock()
		a.succeeded = append(a.succeeded, id)
		a.observations = append(a.observations, settlementObservation{id: id, stateReady: stateReady})
		a.mu.Unlock()
		_ = a.client.Ack(context.Background(), id)
	}
}
func (a *footerIntegrationAcker) TurnFailedIDs(_ string, ids []string, _ error) {
	a.mu.Lock()
	a.failed = append(a.failed, ids...)
	a.mu.Unlock()
}
func (a *footerIntegrationAcker) hasSucceeded(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, got := range a.succeeded {
		if got == id {
			return true
		}
	}
	return false
}
func (a *footerIntegrationAcker) hasFailed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, got := range a.failed {
		if got == id {
			return true
		}
	}
	return false
}
func (a *footerIntegrationAcker) stateWasDeliveredBeforeSuccess(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, observation := range a.observations {
		if observation.id == id {
			return observation.stateReady
		}
	}
	return false
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

// Exercise the complete target dispatcher path: a ready roster replay is
// rate-limited, remains retryable, then an empty model result with footer
// enabled marks the durable roster delivered before the Acker settles it.
func TestCoreA2AReadyRosterRateLimitRetriesThenEmptyFooterSettlesDurably(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "roster-state.json")
	raw := makeTestRoster(t, "saka", 31, "combined dispatch regression")
	manifest, chunks := makeRosterTransferBodies(t, raw, "saka", 31, 1800)
	if len(chunks) != 1 {
		t.Fatalf("got %d roster chunks, want one", len(chunks))
	}
	manifestPiece, err := parseRosterPiece(manifest)
	if err != nil {
		t.Fatal(err)
	}
	finalPiece, err := parseRosterPiece(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	store := newRosterTransferStore(statePath, time.Now)
	if result := store.add("ready-manifest", "saka", "saka", manifestPiece); result.state != rosterReceivePending {
		t.Fatalf("manifest state = %+v, want pending", result)
	}
	if result := store.add("ready-final", "saka", "saka", finalPiece); result.state != rosterReceiveReady {
		t.Fatalf("final chunk state = %+v, want ready", result)
	}
	payload := string(raw)
	key := rosterTransferKey("saka", 31, finalPiece.hash)
	client := &fakeClient{}
	acker := &footerIntegrationAcker{client: client, statePath: statePath, readyRosterID: "ready-final"}
	p := newTestPlatform(t, client, acker, map[string]any{
		"allow_from":    "saka,dot",
		"roster_issuer": "saka",
	})
	p.roster.store = store
	received := make(chan string, 2)
	e := core.NewEngine("a2a-roster-footer-test", &footerTestAgent{response: "", received: received}, []core.Platform{p}, "", core.LangEnglish)
	e.SetReplyFooterEnabled(true)
	e.SetShowContextIndicator(true)
	e.SetRateLimitCfg(core.RateLimitCfg{MaxMessages: 1, Window: time.Minute})
	if err := e.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	ctx := context.Background()
	p.dispatch(ctx, e.ReceiveMessage, in("warmup", "roster-conversation", "saka", "prime the rate limiter"))
	eventually(t, "warmup turn completed", func() bool { return acker.hasSucceeded("warmup") })

	finalMessage := in("ready-final", "roster-conversation", "saka", finalPiece.body)
	p.dispatch(ctx, e.ReceiveMessage, finalMessage)
	p.mu.Lock()
	inflight := p.inflight["ready-final"]
	_, completed := p.done["ready-final"]
	p.mu.Unlock()
	if !acker.hasFailed("ready-final") {
		t.Fatalf("rate-limited ready roster transfer missed retry settlement: inflight=%t completed=%t published=%+v",
			inflight, completed, client.sentCopy())
	}
	if inflight || completed {
		t.Fatalf("rate-limited ready roster release inflight=%t completed=%t, want retryable", inflight, completed)
	}
	if got := len(client.sentCopy()); got != 0 {
		t.Fatalf("rate-limit system response was published to A2A: %+v", client.sentCopy())
	}
	if got := len(received); got != 1 {
		t.Fatalf("agent received %d prompts before roster retry, want only warmup", got)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var pendingState rosterTransferDiskState
	if err := json.Unmarshal(data, &pendingState); err != nil {
		t.Fatal(err)
	}
	if group := pendingState.Groups[key]; group == nil || group.Status != "ready" {
		t.Fatalf("rate-limited roster transfer state = %#v, want durable ready replay", group)
	}

	// Replacing the limiter simulates the next permitted attempt without waiting
	// for a minute. The same transport ID must replay the durable ready payload.
	e.SetRateLimitCfg(core.RateLimitCfg{MaxMessages: 1, Window: time.Minute})
	p.dispatch(ctx, e.ReceiveMessage, finalMessage)
	eventually(t, "roster retry completed", func() bool { return acker.hasSucceeded("ready-final") })
	if !acker.stateWasDeliveredBeforeSuccess("ready-final") {
		t.Fatal("Acker settlement began before durable roster delivery was recorded")
	}
	if got := len(received); got != 2 {
		t.Fatalf("agent received %d prompts after retry, want warmup and roster payload", got)
	}
	if got := <-received; got != "prime the rate limiter" {
		t.Fatalf("first agent prompt = %q", got)
	}
	if got := <-received; got != payload {
		t.Fatalf("replayed roster payload = %q, want %q", got, payload)
	}
	if sent := client.sentCopy(); len(sent) != 0 {
		t.Fatalf("empty model result with footer enabled published a reply: %+v", sent)
	}
}
