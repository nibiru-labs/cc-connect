package a2a

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// ---- fake Client (spec: 共享契约 Client) ----

type sentReply struct {
	ReplyTo string
	Key     string
	Text    string
}

type fakeClient struct {
	mu       sync.Mutex
	pending  []Inbound // returned by every ReadPending until acked (mimics real inbox)
	sent     []sentReply
	acked    []string
	sendErr  error // returned by SendReply when non-nil
	ackErr   error
	readErr  error
	readHits int
}

func (f *fakeClient) ReadPending(ctx context.Context) ([]Inbound, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readHits++
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]Inbound, 0, len(f.pending))
	for _, m := range f.pending {
		done := false
		for _, a := range f.acked {
			if a == m.MessageID {
				done = true
			}
		}
		if !done {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeClient) SendReply(ctx context.Context, replyTo, key, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentReply{replyTo, key, text})
	return nil
}

func (f *fakeClient) Ack(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, id)
	return nil
}

func (f *fakeClient) setSendErr(err error) { f.mu.Lock(); f.sendErr = err; f.mu.Unlock() }
func (f *fakeClient) sentCopy() []sentReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentReply(nil), f.sent...)
}
func (f *fakeClient) ackedCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acked...)
}

// ---- fake Acker (spec: 共享契约 Acker) ----

type fakeAcker struct {
	mu      sync.Mutex
	tracked map[string][]string
	ok      []string
	failed  []string
}

func newFakeAcker() *fakeAcker { return &fakeAcker{tracked: map[string][]string{}} }

func (a *fakeAcker) Track(key string, ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tracked[key] = append(a.tracked[key], ids...)
}
func (a *fakeAcker) TurnSucceeded(key string) { a.mu.Lock(); a.ok = append(a.ok, key); a.mu.Unlock() }
func (a *fakeAcker) TurnFailed(key string, err error) {
	a.mu.Lock()
	a.failed = append(a.failed, key)
	a.mu.Unlock()
}

// ---- recording handler ----

type recorder struct {
	mu   sync.Mutex
	msgs []*core.Message
}

func (r *recorder) handle(p core.Platform, m *core.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}
func (r *recorder) all() []*core.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*core.Message(nil), r.msgs...)
}

// ---- adapters to L1/L2 implementation (single place to fix if names differ) ----

// newTestPlatform builds the platform under test with an injected Client and Acker.
// TODO(836-L1): assumes `newWithClient(opts map[string]any, c Client, a Acker) (*Platform, error)`
// (acker may be nil => platform builds the real L2 acker itself).
func newTestPlatform(t *testing.T, c Client, a Acker, extra map[string]any) *Platform {
	t.Helper()
	opts := map[string]any{
		"poll_interval": "10ms",
		"allow_from":    "grok,dot,claude-cloud,hermes",
	}
	for k, v := range extra {
		opts[k] = v
	}
	p, err := newWithClient(opts, c, a)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	return p
}

// newTestAcker builds the real L2 Acker.
// TODO(836-L2): assumes `NewAcker(c Client, maxRetries int, backoff time.Duration) Acker`.
func newTestAcker(c Client, maxRetries int) Acker {
	return NewAcker(c, maxRetries, time.Millisecond)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

var errBoom = errors.New("boom")
