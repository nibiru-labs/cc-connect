package core

import (
	"net/http"
	"reflect"
	"time"
)

// RuntimeSnapshotProvider is optional. Alive connections alone do not imply
// active work. Implementations return metadata only, never message content.
type RuntimeSnapshotProvider interface{ RuntimeSnapshot() RuntimeSnapshot }

// RuntimeSnapshot describes one protocol turn, not its transport or account.
type RuntimeSnapshot struct {
	ThreadID   string    `json:"thread_id,omitempty"`
	TurnID     string    `json:"turn_id,omitempty"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	ObservedAt time.Time `json:"observed_at"`
	Alive      bool      `json:"alive"`
	Complete   bool      `json:"complete"`
}

type RuntimeResponse struct {
	Type               string            `json:"type"`
	ObservedAt         time.Time         `json:"observed_at"`
	Complete           bool              `json:"complete"`
	ActualRunningTurns *int              `json:"actual_running_turns"`
	Sessions           []RuntimeSnapshot `json:"sessions"`
}

func validRuntimeSnapshot(s RuntimeSnapshot, now time.Time) bool {
	if !s.Complete || s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) > 5*time.Second || s.ObservedAt.After(now.Add(time.Second)) {
		return false
	}
	switch s.Status {
	case "closed":
		return !s.Alive && s.TurnID == "" && s.StartedAt.IsZero()
	case "idle":
		return s.Alive && s.ThreadID != "" && s.TurnID == "" && s.StartedAt.IsZero()
	case "running", "waiting_input":
		return s.Alive && s.ThreadID != "" && s.TurnID != "" && !s.StartedAt.IsZero() && !s.StartedAt.After(s.ObservedAt)
	default:
		return false
	}
}

func (s *APIServer) handleRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Snapshot references under the existing registry/state locks, then release
	// them before calling optional agent code. No keys or project names escape.
	s.mu.RLock()
	var sessions []AgentSession
	seen := make(map[AgentSession]bool)
	for _, e := range s.engines {
		e.interactiveMu.Lock()
		for _, state := range e.interactiveStates {
			state.mu.Lock()
			session := state.agentSession
			state.mu.Unlock()
			if session != nil {
				// Session implementations normally use pointers. Preserve
				// compatibility with non-comparable optional implementations.
				if reflect.TypeOf(session).Comparable() {
					if seen[session] {
						continue
					}
					seen[session] = true
				}
				sessions = append(sessions, session)
			}
		}
		e.interactiveMu.Unlock()
	}
	s.mu.RUnlock()
	now := time.Now().UTC()
	result := RuntimeResponse{Type: "cc-connect.runtime.v1", ObservedAt: now, Complete: true, Sessions: []RuntimeSnapshot{}}
	running := make(map[[2]string]bool)
	for _, session := range sessions {
		snapshot := RuntimeSnapshot{Status: "unknown", ObservedAt: now}
		if provider, ok := session.(RuntimeSnapshotProvider); ok {
			snapshot = provider.RuntimeSnapshot()
		}
		if !validRuntimeSnapshot(snapshot, time.Now().UTC()) {
			snapshot.Complete = false
			result.Complete = false
		}
		result.Sessions = append(result.Sessions, snapshot)
		if snapshot.Complete && snapshot.Status == "running" {
			running[[2]string{snapshot.ThreadID, snapshot.TurnID}] = true
		}
	}
	if result.Complete {
		n := len(running)
		result.ActualRunningTurns = &n
	}
	apiJSON(w, http.StatusOK, result)
}
