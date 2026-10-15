package a2a

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type ackFakeClient struct {
	mu      sync.Mutex
	acked   []string
	replies []ackReply
	ackErr  error
	sendErr error
}

type ackReply struct{ replyTo, key, text string }

func (c *ackFakeClient) ReadPending(context.Context) ([]Inbound, error) { return nil, nil }
func (c *ackFakeClient) SendReply(_ context.Context, replyTo, key, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.replies = append(c.replies, ackReply{replyTo, key, text})
	return nil
}
func (c *ackFakeClient) Ack(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ackErr != nil {
		return c.ackErr
	}
	c.acked = append(c.acked, id)
	return nil
}
func (c *ackFakeClient) ackedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.acked...)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestAcker(c Client, alerts *[]string) (*DefaultAcker, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	a := NewAcker(c, AckerConfig{
		MaxAttempts: 3,
		Backoff:     func(n int) time.Duration { return time.Duration(n) * time.Second },
		Now:         clk.now,
		Alert: func(key string, ids []string, err error) {
			*alerts = append(*alerts, key)
		},
		DisableAckRetry: true,
	})
	return a, clk
}

func TestAckerSuccessAcksAllTracked(t *testing.T) {
	c := &ackFakeClient{}
	var alerts []string
	a, _ := newTestAcker(c, &alerts)
	a.Track("a2a:c1", []string{"m1", "m2"})
	a.Track("a2a:c1", []string{"m2", "m3"}) // duplicate m2 ignored
	a.TurnSucceeded("a2a:c1")
	got := c.ackedIDs()
	if len(got) != 3 || got[0] != "m1" || got[1] != "m2" || got[2] != "m3" {
		t.Fatalf("acked = %v", got)
	}
	a.TurnSucceeded("a2a:c1") // second call is a no-op
	if len(c.ackedIDs()) != 3 {
		t.Fatalf("double ack: %v", c.ackedIDs())
	}
}

func TestAckerFailureDoesNotAckAndBacksOff(t *testing.T) {
	c := &ackFakeClient{}
	var alerts []string
	a, clk := newTestAcker(c, &alerts)
	a.Track("k", []string{"m1"})
	if a.Admit("m1") {
		t.Fatal("in-flight message must not be admitted again")
	}
	a.TurnFailed("k", errors.New("boom"))
	if len(c.ackedIDs()) != 0 {
		t.Fatal("must not ack on failure")
	}
	if a.Admit("m1") {
		t.Fatal("must wait for backoff")
	}
	clk.advance(1 * time.Second)
	if !a.Admit("m1") {
		t.Fatal("backoff elapsed: should be redelivered")
	}
	a.Track("k", []string{"m1"})
	a.TurnFailed("k", errors.New("boom"))
	clk.advance(1500 * time.Millisecond) // backoff for attempt 2 is 2s
	if a.Admit("m1") {
		t.Fatal("backoff grows with attempts")
	}
	clk.advance(1 * time.Second)
	if !a.Admit("m1") {
		t.Fatal("second backoff elapsed")
	}
	if len(alerts) != 0 {
		t.Fatalf("unexpected alerts %v", alerts)
	}
}

func TestAckerGivesUpAfterMaxAttempts(t *testing.T) {
	c := &ackFakeClient{}
	var alerts []string
	a, clk := newTestAcker(c, &alerts)
	for i := 0; i < 3; i++ {
		clk.advance(time.Minute)
		if !a.Admit("m1") {
			t.Fatalf("round %d: should admit", i)
		}
		a.Track("k", []string{"m1"})
		a.TurnFailed("k", errors.New("boom"))
	}
	if got := c.ackedIDs(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("poison message must be acked after max attempts, got %v", got)
	}
	if len(alerts) != 1 || alerts[0] != "k" {
		t.Fatalf("alerts = %v", alerts)
	}
}

func TestAckerSuccessClearsFailureCount(t *testing.T) {
	c := &ackFakeClient{}
	var alerts []string
	a, clk := newTestAcker(c, &alerts)
	a.Track("k", []string{"m1"})
	a.TurnFailed("k", errors.New("x"))
	clk.advance(time.Minute)
	a.Admit("m1")
	a.Track("k", []string{"m1"})
	a.TurnSucceeded("k")
	if len(c.ackedIDs()) != 1 {
		t.Fatal("should ack")
	}
	if !a.Admit("m1") {
		t.Fatal("state should be cleared after ack")
	}
}

func TestAckerAckErrorKeepsMessageForRetry(t *testing.T) {
	c := &ackFakeClient{ackErr: errors.New("net")}
	var alerts []string
	a, _ := newTestAcker(c, &alerts)
	a.Track("k", []string{"m1"})
	a.TurnSucceeded("k")
	if len(c.ackedIDs()) != 0 {
		t.Fatal("ack failed")
	}
	// A failed ack leaves the message server-side; it must be retried, not re-run through the model.
	c.mu.Lock()
	c.ackErr = nil
	c.mu.Unlock()
	a.RetryAcks(context.Background())
	if got := c.ackedIDs(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("retry acked %v", got)
	}
	if !a.Admit("m1") {
		t.Fatal("after successful retry the id is released")
	}
}

func TestAckerConcurrent(t *testing.T) {
	c := &ackFakeClient{}
	var alerts []string
	a, _ := newTestAcker(c, &alerts)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+i%5))
			id := "m" + string(rune('A'+i%26)) + string(rune('a'+i/26))
			a.Track(key, []string{id})
			if i%2 == 0 {
				a.TurnSucceeded(key)
			} else {
				a.TurnFailed(key, errors.New("e"))
			}
		}(i)
	}
	wg.Wait()
}
