package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"strings"
	"sync"
	"testing"
	"time"
)

func runtimeTestSession() *appServerSession {
	s := &appServerSession{events: make(chan core.Event, 64), pendingApprovals: make(map[string]chan core.PermissionResult)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.threadID.Store("thread-opaque")
	s.alive.Store(true)
	return s
}
func TestRuntimeSnapshotTurnLifecycle(t *testing.T) {
	s := runtimeTestSession()
	if got := s.RuntimeSnapshot(); got.Status != "idle" || !got.Complete {
		t.Fatalf("initial: %+v", got)
	}
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-1"}}`))
	first := s.RuntimeSnapshot()
	if first.Status != "running" || first.TurnID != "turn-1" || first.StartedAt.IsZero() {
		t.Fatalf("started: %+v", first)
	}
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-1"}}`))
	if !s.RuntimeSnapshot().StartedAt.Equal(first.StartedAt) {
		t.Fatal("duplicate reset start")
	}
	s.setRuntimeWaiting("approval", true)
	if s.RuntimeSnapshot().Status != "waiting_input" {
		t.Fatal("approval counted running")
	}
	s.setRuntimeWaiting("approval", false)
	if s.RuntimeSnapshot().Status != "running" {
		t.Fatal("approval did not resume")
	}
	for _, method := range []string{"turn/completed", "thread/status/changed"} {
		s.handleNotification(method, json.RawMessage(`{"turn":{"id":"turn-1"},"status":{"type":"idle"}}`))
		if got := s.RuntimeSnapshot(); got.Status != "idle" || got.TurnID != "" || !got.StartedAt.IsZero() {
			t.Fatalf("completed: %+v", got)
		}
	}
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-2"}}`))
	s.handleNotification("turn/completed", json.RawMessage(`{"turn":{"id":"turn-2","status":"failed"}}`))
	if s.RuntimeSnapshot().Status != "idle" {
		t.Fatal("failed counted running")
	}
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-3"}}`))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.RuntimeSnapshot(); got.Status != "closed" || got.Alive || got.TurnID != "" {
		t.Fatalf("closed: %+v", got)
	}
}
func TestRuntimeSnapshotLateStartCannotResurrectCompletedTurn(t *testing.T) {
	s := runtimeTestSession()
	epoch := s.runtimeEpoch
	s.handleNotification("turn/completed", json.RawMessage(`{"turn":{"id":"finished"}}`))
	s.recordTurnStartResponse("finished", epoch)
	if got := s.RuntimeSnapshot(); got.Status != "idle" || got.TurnID != "" {
		t.Fatalf("late response resurrected: %+v", got)
	}
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"finished"}}`))
	if s.RuntimeSnapshot().Status != "idle" {
		t.Fatal("late duplicate started resurrected")
	}
}
func TestRuntimeSnapshotConcurrentReadsAndTransitions(t *testing.T) {
	s := runtimeTestSession()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.handleNotification("turn/started", json.RawMessage(fmt.Sprintf(`{"turn":{"id":"t%d"}}`, i)))
			s.completeTurn()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			got := s.RuntimeSnapshot()
			if got.ObservedAt.Before(time.Now().Add(-time.Second)) {
				t.Error("stale")
			}
		}
	}()
	wg.Wait()
}

func TestRuntimeSnapshotOlderTurnAndForeignThreadCannotEndCurrent(t *testing.T) {
	s := runtimeTestSession()
	epoch := s.runtimeEpoch
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"a"}}`))
	s.handleNotification("turn/completed", json.RawMessage(`{"turn":{"id":"a"}}`))
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"b"}}`))
	s.stateMu.Lock()
	s.pendingMsgs = []string{"preserved text"}
	s.stateMu.Unlock()
	first := s.RuntimeSnapshot()
	s.recordTurnStartResponse("a", epoch)
	for _, raw := range []string{`{"turn":{"id":"a"}}`, `{"turn":{"id":"a","status":"failed"}}`, `{"threadId":"foreign","turn":{"id":"b"}}`} {
		s.handleNotification("turn/completed", json.RawMessage(raw))
	}
	s.handleNotification("thread/status/changed", json.RawMessage(`{"threadId":"foreign","status":{"type":"idle"}}`))
	s.handleNotification("turn/started", json.RawMessage(`{"threadId":"foreign","turn":{"id":"c"}}`))
	s.recordTurnStartResponse("b", s.runtimeEpoch)
	if got := s.RuntimeSnapshot(); got.TurnID != "b" || !got.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("old event mutated current %+v", got)
	}
	if len(s.pendingMsgs) != 1 {
		t.Fatal("duplicate discarded text")
	}
	s.setRuntimeWaiting("x", true)
	s.setRuntimeWaiting("y", true)
	s.setRuntimeWaiting("x", false)
	if s.RuntimeSnapshot().Status != "waiting_input" {
		t.Fatal("one of two approvals remains")
	}
	s.setRuntimeWaiting("y", false)
	if s.RuntimeSnapshot().Status != "running" {
		t.Fatal("all approvals resolved")
	}
}
func TestRuntimeSnapshotTransportEOFAndLateEvents(t *testing.T) {
	s := runtimeTestSession()
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"active"}}`))
	s.wg.Add(1)
	s.readLoop(strings.NewReader(""))
	s.recordTurnStartResponse("late", s.runtimeEpoch)
	s.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"later"}}`))
	got := s.RuntimeSnapshot()
	if got.Status != "closed" || got.Alive || got.TurnID != "" || !got.StartedAt.IsZero() {
		t.Fatalf("EOF snapshot %+v", got)
	}
}
