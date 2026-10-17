package dingtalk

import (
	"testing"
	"time"
)

// The reconnect backoff is exponential from 3s and capped at 60s: a long outage
// must not hammer the gateway, but the first retry stays fast.
func TestStreamReconnectDelay(t *testing.T) {
	want := []time.Duration{
		0,                // no failure: nothing to back off from
		3 * time.Second,  // first failure keeps the historical 3s pause
		6 * time.Second,  // doubles
		12 * time.Second, //
		24 * time.Second, //
		48 * time.Second, //
		60 * time.Second, // 96s would exceed the cap
		60 * time.Second, // stays capped
		60 * time.Second, //
	}
	for failures, w := range want {
		if got := streamReconnectDelay(failures); got != w {
			t.Fatalf("streamReconnectDelay(%d) = %s, want %s", failures, got, w)
		}
	}
}

func TestStreamReconnectDelay_NegativeIsNoDelay(t *testing.T) {
	if got := streamReconnectDelay(-1); got != 0 {
		t.Fatalf("streamReconnectDelay(-1) = %s, want 0", got)
	}
}

// A reconnect resets the backoff: the first failure after a healthy stretch
// waits 3s again, not the accumulated 60s.
func TestStreamReconnectDelay_ResetsAfterSuccessfulStart(t *testing.T) {
	failedAttempts := 0
	// 5 failures in a row -> 48s delay.
	for i := 0; i < 5; i++ {
		failedAttempts++
		_ = streamReconnectDelay(failedAttempts)
	}
	if got := streamReconnectDelay(failedAttempts); got != 48*time.Second {
		t.Fatalf("after 5 failures delay = %s, want 48s", got)
	}
	// Start() returned nil -> counter reset by the restart loop.
	failedAttempts = 0
	if got := streamReconnectDelay(failedAttempts + 1); got != streamHealthyInterval {
		t.Fatalf("first delay after reconnect = %s, want %s", got, streamHealthyInterval)
	}
}

// While connected the loop keeps the historical 3s cadence.
func TestStreamHealthyInterval(t *testing.T) {
	if streamHealthyInterval != 3*time.Second {
		t.Fatalf("streamHealthyInterval = %s, want 3s", streamHealthyInterval)
	}
	if streamReconnectMax != 60*time.Second {
		t.Fatalf("streamReconnectMax = %s, want 60s", streamReconnectMax)
	}
	if streamReconnectBase != 3*time.Second {
		t.Fatalf("streamReconnectBase = %s, want 3s", streamReconnectBase)
	}
}
