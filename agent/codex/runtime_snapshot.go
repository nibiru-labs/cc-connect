package codex

import (
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// RuntimeSnapshot samples protocol state under the same lock used by turn
// transitions. It does not inspect transcripts, prompts, config, or accounts.
func (s *appServerSession) RuntimeSnapshot() core.RuntimeSnapshot {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	now := time.Now().UTC()
	snapshot := core.RuntimeSnapshot{ThreadID: s.CurrentSessionID(), ObservedAt: now, Alive: s.alive.Load(), Complete: true, Status: "idle"}
	if !snapshot.Alive {
		snapshot.Status = "closed"
		return snapshot
	}
	if snapshot.ThreadID == "" {
		snapshot.Status = "unknown"
		snapshot.Complete = false
		return snapshot
	}
	if s.currentTurn != "" {
		snapshot.TurnID = s.currentTurn
		snapshot.StartedAt = s.turnStartedAt
		snapshot.Status = "running"
		for _, turn := range s.runtimeWaiting {
			if turn == s.currentTurn {
				snapshot.Status = "waiting_input"
				break
			}
		}
		if snapshot.StartedAt.IsZero() {
			snapshot.Status = "unknown"
			snapshot.Complete = false
		}
	}
	return snapshot
}

func (s *appServerSession) startTurnLocked(id string) {
	if id == "" || id == s.lastFinishedTurn || s.runtimeClosed {
		return
	}
	if s.currentTurn != id {
		s.currentTurn = id
		s.turnStartedAt = time.Now().UTC()
		for request, turn := range s.runtimeWaiting {
			if turn == "" {
				s.runtimeWaiting[request] = id
			}
		}
		s.pendingMsgs = s.pendingMsgs[:0]
	}
}
func (s *appServerSession) recordTurnStartResponse(id string, epoch uint64) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.runtimeEpoch == epoch {
		s.startTurnLocked(id)
	}
}
func (s *appServerSession) setRuntimeWaiting(id string, waiting bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if waiting {
		if s.runtimeClosed {
			return
		}
		if s.runtimeWaiting == nil {
			s.runtimeWaiting = make(map[string]string)
		}
		s.runtimeWaiting[id] = s.currentTurn
	} else {
		delete(s.runtimeWaiting, id)
	}
}

// finishTurnLocked also records early terminal events, before a start reply.
func (s *appServerSession) finishTurnLocked(id string) bool {
	if id != "" && s.currentTurn != "" && id != s.currentTurn {
		return false
	}
	s.runtimeEpoch++
	if id != "" {
		s.lastFinishedTurn = id
	} else if s.currentTurn != "" {
		s.lastFinishedTurn = s.currentTurn
	}
	active := s.currentTurn != ""
	s.currentTurn = ""
	s.turnStartedAt = time.Time{}
	return active
}

func (s *appServerSession) markRuntimeClosed() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.alive.Store(false)
	s.runtimeClosed = true
	s.runtimeEpoch++
	s.currentTurn = ""
	s.turnStartedAt = time.Time{}
	s.runtimeWaiting = nil
}
