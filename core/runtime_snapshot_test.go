package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type runtimeStubSession struct {
	stubAgentSession
	snapshot RuntimeSnapshot
}

func (s *runtimeStubSession) RuntimeSnapshot() RuntimeSnapshot { return s.snapshot }
func TestRuntimeEndpointCountDedupAndPrivacy(t *testing.T) {
	now := time.Now().UTC()
	session := &runtimeStubSession{snapshot: RuntimeSnapshot{ThreadID: "opaque-thread", TurnID: "turn", Status: "running", StartedAt: now, ObservedAt: now, Alive: true, Complete: true}}
	e := &Engine{interactiveStates: map[string]*interactiveState{"PRIVATE_SESSION_KEY": {agentSession: session}, "alias": {agentSession: session}}}
	a := &APIServer{engines: map[string]*Engine{"PRIVATE_PROJECT": e}}
	rec := httptest.NewRecorder()
	a.handleRuntime(rec, httptest.NewRequest(http.MethodGet, "/runtime", nil))
	var got RuntimeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 1 {
		t.Fatalf("result=%s", rec.Body)
	}
	for _, secret := range []string{"PRIVATE_SESSION_KEY", "PRIVATE_PROJECT", "session_key", "prompt", "account", "config"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	e.interactiveStates["unsupported"] = &interactiveState{agentSession: &stubAgentSession{}}
	rec = httptest.NewRecorder()
	a.handleRuntime(rec, httptest.NewRequest(http.MethodGet, "/runtime", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Complete || got.ActualRunningTurns != nil {
		t.Fatalf("unsupported must unknown: %s", rec.Body)
	}
}
func TestRuntimeEndpointGetOnlyAndIncomplete(t *testing.T) {
	a := &APIServer{engines: map[string]*Engine{}}
	rec := httptest.NewRecorder()
	a.handleRuntime(rec, httptest.NewRequest(http.MethodPost, "/runtime", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatal(rec.Code)
	}
	for _, snapshot := range []RuntimeSnapshot{
		{Status: "running", Alive: true, Complete: true},
		{ThreadID: "t", TurnID: "r", Status: "running", Alive: true, Complete: true, StartedAt: time.Now(), ObservedAt: time.Now().Add(-time.Hour)},
		{Status: "unknown", Complete: false},
	} {
		a.engines = map[string]*Engine{"test": {interactiveStates: map[string]*interactiveState{"s": {agentSession: &runtimeStubSession{snapshot: snapshot}}}}}
		rec = httptest.NewRecorder()
		a.handleRuntime(rec, httptest.NewRequest(http.MethodGet, "/runtime", nil))
		var got RuntimeResponse
		json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Complete || got.ActualRunningTurns != nil {
			t.Fatalf("invalid source=%s", rec.Body)
		}
	}
}

func TestRuntimeEndpointUnenumerableSessionIsIncomplete(t *testing.T) {
	cases := map[string]map[string]*Engine{
		"nil engine": {"project": nil},
		"nil interactive state": {
			"project": {interactiveStates: map[string]*interactiveState{"session": nil}},
		},
		"missing agent session": {
			"project": {interactiveStates: map[string]*interactiveState{"session": {}}},
		},
		"typed nil agent session": {
			"project": {interactiveStates: map[string]*interactiveState{"session": {agentSession: (*runtimeStubSession)(nil)}}},
		},
	}
	for name, engines := range cases {
		t.Run(name, func(t *testing.T) {
			api := &APIServer{engines: engines}
			rec := httptest.NewRecorder()
			api.handleRuntime(rec, httptest.NewRequest(http.MethodGet, "/runtime", nil))
			var got RuntimeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Complete || got.ActualRunningTurns != nil {
				t.Fatalf("unenumerable session must be unknown, response=%s", rec.Body)
			}
		})
	}
}

func TestRuntimeEndpointStatusesMethodsAndEmptyInventory(t *testing.T) {
	a := &APIServer{engines: map[string]*Engine{}}
	for _, method := range []string{"HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		rec := httptest.NewRecorder()
		a.handleRuntime(rec, httptest.NewRequest(method, "/runtime", nil))
		if rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
			t.Fatalf("method %s: %d", method, rec.Code)
		}
	}
	for _, status := range []string{"idle", "closed", "waiting_input"} {
		now := time.Now().UTC()
		snapshot := RuntimeSnapshot{ThreadID: "thread", Status: status, ObservedAt: now, Complete: true, Alive: status != "closed"}
		if status == "waiting_input" {
			snapshot.TurnID = "turn"
			snapshot.StartedAt = now
		}
		a.engines = map[string]*Engine{"test": {interactiveStates: map[string]*interactiveState{"key": {agentSession: &runtimeStubSession{snapshot: snapshot}}}}}
		rec := httptest.NewRecorder()
		a.handleRuntime(rec, httptest.NewRequest("GET", "/runtime", nil))
		var got RuntimeResponse
		json.Unmarshal(rec.Body.Bytes(), &got)
		if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 0 {
			t.Fatalf("%s=%s", status, rec.Body)
		}
	}
	a.engines = map[string]*Engine{}
	rec := httptest.NewRecorder()
	a.handleRuntime(rec, httptest.NewRequest("GET", "/runtime", nil))
	var raw map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if string(raw["sessions"]) != "[]" || string(raw["actual_running_turns"]) != "0" {
		t.Fatal(rec.Body)
	}
	expected := map[string]bool{"type": true, "observed_at": true, "complete": true, "actual_running_turns": true, "sessions": true}
	for key := range raw {
		if !expected[key] {
			t.Fatalf("unapproved field %s", key)
		}
	}
}

type rotatingRuntimeSession struct {
	stubAgentSession
	calls int
}

func (s *rotatingRuntimeSession) RuntimeSnapshot() RuntimeSnapshot {
	s.calls++
	now := time.Now().UTC()
	return RuntimeSnapshot{ThreadID: "one-thread", TurnID: fmt.Sprint(s.calls), Status: "running", StartedAt: now, ObservedAt: now, Alive: true, Complete: true}
}
func TestRuntimeEndpointSamplesAliasedSessionOnce(t *testing.T) {
	session := &rotatingRuntimeSession{}
	api := &APIServer{engines: map[string]*Engine{"p": {interactiveStates: map[string]*interactiveState{"a": {agentSession: session}, "b": {agentSession: session}}}}}
	rec := httptest.NewRecorder()
	api.handleRuntime(rec, httptest.NewRequest("GET", "/runtime", nil))
	var got RuntimeResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if session.calls != 1 || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 1 {
		t.Fatalf("calls=%d response=%s", session.calls, rec.Body)
	}
}

func TestRuntimeEndpointTupleIdentityAndInvalidMetadata(t *testing.T) {
	now := time.Now().UTC()
	snapshots := []RuntimeSnapshot{
		{ThreadID: "a:b", TurnID: "c", Status: "running", StartedAt: now, ObservedAt: now, Alive: true, Complete: true},
		{ThreadID: "a", TurnID: "b:c", Status: "running", StartedAt: now, ObservedAt: now, Alive: true, Complete: true},
		{ThreadID: "a:b", TurnID: "c", Status: "running", StartedAt: now, ObservedAt: now, Alive: true, Complete: true},
	}
	e := &Engine{interactiveStates: map[string]*interactiveState{}}
	for i, snapshot := range snapshots {
		e.interactiveStates[fmt.Sprint(i)] = &interactiveState{agentSession: &runtimeStubSession{snapshot: snapshot}}
	}
	api := &APIServer{engines: map[string]*Engine{"p": e}}
	rec := httptest.NewRecorder()
	api.handleRuntime(rec, httptest.NewRequest("GET", "/runtime", nil))
	var got RuntimeResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 2 {
		t.Fatal(rec.Body)
	}
	for _, mutate := range []func(*RuntimeSnapshot){
		func(s *RuntimeSnapshot) { s.ObservedAt = time.Time{} },
		func(s *RuntimeSnapshot) { s.StartedAt = now.Add(time.Hour) },
		func(s *RuntimeSnapshot) { s.Alive = false },
		func(s *RuntimeSnapshot) { s.Status = "unsupported-state" },
	} {
		snapshot := snapshots[0]
		mutate(&snapshot)
		if validRuntimeSnapshot(snapshot, now) {
			t.Fatalf("accepted invalid %+v", snapshot)
		}
	}
}

// blockingRelaySnapshotSession is a synthetic AgentSession used only by this
// regression test. It models an in-flight relay turn without starting a real
// agent process or contacting a model provider.
type blockingRelaySnapshotSession struct {
	mu        sync.Mutex
	events    chan Event
	sendReady chan struct{}
	closedCh  chan struct{}
	closed    bool
}

func newBlockingRelaySnapshotSession() *blockingRelaySnapshotSession {
	return &blockingRelaySnapshotSession{
		events:    make(chan Event, 1),
		sendReady: make(chan struct{}),
		closedCh:  make(chan struct{}),
	}
}

func (s *blockingRelaySnapshotSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	close(s.sendReady)
	return nil
}
func (s *blockingRelaySnapshotSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *blockingRelaySnapshotSession) Events() <-chan Event                             { return s.events }
func (s *blockingRelaySnapshotSession) CurrentSessionID() string                         { return "synthetic-relay-thread" }
func (s *blockingRelaySnapshotSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}
func (s *blockingRelaySnapshotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
		close(s.closedCh)
	}
	return nil
}
func (s *blockingRelaySnapshotSession) RuntimeSnapshot() RuntimeSnapshot {
	now := time.Now().UTC()
	if !s.Alive() {
		return RuntimeSnapshot{Status: "closed", ObservedAt: now, Complete: true}
	}
	return RuntimeSnapshot{
		ThreadID:   "synthetic-relay-thread",
		TurnID:     "synthetic-relay-turn",
		Status:     "running",
		StartedAt:  now,
		ObservedAt: now,
		Alive:      true,
		Complete:   true,
	}
}

func runtimeResponseForTest(t *testing.T, e *Engine) (RuntimeResponse, string) {
	t.Helper()
	got, body, err := runtimeResponse(e)
	if err != nil {
		t.Fatalf("decode runtime response: %v", err)
	}
	return got, body
}

func runtimeResponse(e *Engine) (RuntimeResponse, string, error) {
	api := &APIServer{engines: map[string]*Engine{"PRIVATE_PROJECT": e}}
	rec := httptest.NewRecorder()
	api.handleRuntime(rec, httptest.NewRequest(http.MethodGet, "/runtime", nil))
	var got RuntimeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		return RuntimeResponse{}, rec.Body.String(), err
	}
	return got, rec.Body.String(), nil
}

func TestRuntimeEndpointCountsSyntheticInFlightRelay(t *testing.T) {
	// This regression is entirely synthetic: HandleRelay is exercised with a
	// blocking fake session; no production model or host is involved.
	e := newTestEngine()
	session := newBlockingRelaySnapshotSession()
	e.agent = &controllableAgent{nextSession: session}

	done := make(chan error, 1)
	go func() {
		_, err := e.HandleRelay(context.Background(), "PRIVATE_SOURCE", "test:chat-1:user", "PRIVATE_PROMPT")
		done <- err
	}()
	<-session.sendReady

	got, body := runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 1 {
		t.Fatalf("in-flight synthetic relay must be counted; response=%s", body)
	}
	for _, privateValue := range []string{"PRIVATE_PROJECT", "PRIVATE_SOURCE", "PRIVATE_PROMPT", "test:chat-1:user"} {
		if strings.Contains(body, privateValue) {
			t.Fatalf("runtime response leaked %q: %s", privateValue, body)
		}
	}

	session.events <- Event{Type: EventResult, Content: "synthetic completion", Done: true}
	if err := <-done; err != nil {
		t.Fatalf("HandleRelay() error = %v", err)
	}
	got, body = runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 0 {
		t.Fatalf("completed synthetic relay must leave no in-flight count; response=%s", body)
	}
}

func TestRuntimeEndpointUnknownRelayStartIsIncomplete(t *testing.T) {
	e := newTestEngine()
	startReady := make(chan struct{})
	allowStartReturn := make(chan struct{})
	e.agent = &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		close(startReady)
		<-allowStartReturn
		return nil, fmt.Errorf("synthetic StartSession failure")
	}}

	done := make(chan error, 1)
	go func() {
		_, err := e.HandleRelay(context.Background(), "PRIVATE_SOURCE", "test:chat-1:user", "PRIVATE_PROMPT")
		done <- err
	}()
	<-startReady
	got, body := runtimeResponseForTest(t, e)
	if got.Complete || got.ActualRunningTurns != nil {
		t.Fatalf("unresolved relay StartSession must be incomplete, response=%s", body)
	}
	close(allowStartReturn)
	if err := <-done; err == nil {
		t.Fatal("HandleRelay() error = nil, want synthetic start failure")
	}
	got, body = runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 0 {
		t.Fatalf("failed relay start must be removed from inventory, response=%s", body)
	}
}

func TestRuntimeEndpointRemovesFailedRelay(t *testing.T) {
	e := newTestEngine()
	session := newBlockingRelaySnapshotSession()
	e.agent = &controllableAgent{nextSession: session}
	done := make(chan error, 1)
	go func() {
		_, err := e.HandleRelay(context.Background(), "PRIVATE_SOURCE", "test:chat-1:user", "PRIVATE_PROMPT")
		done <- err
	}()
	<-session.sendReady

	got, body := runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 1 {
		t.Fatalf("failed synthetic relay should be counted while active, response=%s", body)
	}
	session.events <- Event{Type: EventError, Error: fmt.Errorf("synthetic relay failure")}
	if err := <-done; err == nil || err.Error() != "synthetic relay failure" {
		t.Fatalf("HandleRelay() error = %v, want synthetic relay failure", err)
	}
	got, body = runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 0 {
		t.Fatalf("failed synthetic relay must leave no inventory entry, response=%s", body)
	}
}

func TestRuntimeEndpointKeepsCanceledRelayVisibleUntilDrainCompletes(t *testing.T) {
	e := newTestEngine()
	session := newBlockingRelaySnapshotSession()
	e.agent = &controllableAgent{nextSession: session}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.HandleRelay(ctx, "PRIVATE_SOURCE", "test:chat-1:user", "PRIVATE_PROMPT")
		done <- err
	}()
	<-session.sendReady

	cancel()
	// HandleRelay checks cancellation after receiving an event and then hands
	// the still-live session to its background drain goroutine.
	session.events <- Event{Type: EventThinking, Content: "synthetic progress"}
	if err := <-done; err == nil {
		t.Fatal("HandleRelay() error = nil, want canceled relay error")
	}
	got, body := runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 1 {
		t.Fatalf("canceled relay in background drain must remain counted, response=%s", body)
	}
	session.events <- Event{Type: EventResult, Content: "synthetic completion", Done: true}
	select {
	case <-session.closedCh:
	case <-time.After(time.Second):
		t.Fatal("synthetic relay drain did not close the session")
	}
	got, body = runtimeResponseForTest(t, e)
	if !got.Complete || got.ActualRunningTurns == nil || *got.ActualRunningTurns != 0 {
		t.Fatalf("drained synthetic relay must be removed, response=%s", body)
	}
}

func TestRuntimeEndpointConcurrentRelaySnapshotsDoNotLeak(t *testing.T) {
	e := newTestEngine()
	session := newBlockingRelaySnapshotSession()
	e.agent = &controllableAgent{nextSession: session}
	done := make(chan error, 1)
	go func() {
		_, err := e.HandleRelay(context.Background(), "PRIVATE_SOURCE", "test:chat-1:user", "PRIVATE_PROMPT")
		done <- err
	}()
	<-session.sendReady

	const readers = 8
	const samples = 20
	start := make(chan struct{})
	errs := make(chan error, readers*samples)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < samples; j++ {
				got, body, err := runtimeResponse(e)
				if err != nil {
					errs <- err
					continue
				}
				if !got.Complete || got.ActualRunningTurns == nil {
					errs <- fmt.Errorf("synthetic runtime source unexpectedly incomplete: %s", body)
				}
				for _, privateValue := range []string{"PRIVATE_PROJECT", "PRIVATE_SOURCE", "PRIVATE_PROMPT", "test:chat-1:user"} {
					if strings.Contains(body, privateValue) {
						errs <- fmt.Errorf("runtime response leaked private value %q", privateValue)
					}
				}
			}
		}()
	}
	close(start)
	session.events <- Event{Type: EventResult, Content: "synthetic completion", Done: true}
	if err := <-done; err != nil {
		t.Fatalf("HandleRelay() error = %v", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
