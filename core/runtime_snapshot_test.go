package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
