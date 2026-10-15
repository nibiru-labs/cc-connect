// Package queue is the JSON-file-backed state shared by the fake a2a CLI
// (which cc-connect's a2a platform shells out to) and the e2e tests.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Msg mirrors a2a.Inbound (`a2a read --pending`).
type Msg struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	SenderID       string `json:"sender_id"`
	InReplyTo      string `json:"in_reply_to"`
	Text           string `json:"text"`
}

// Sent is one recorded `a2a send`.
type Sent struct {
	ReplyTo string `json:"reply_to"`
	Key     string `json:"key"`
	Text    string `json:"text"`
}

// State is the whole fake-server state.
type State struct {
	Pending []Msg    `json:"pending"` // not yet acked, in delivery order
	Acked   []string `json:"acked"`
	Sent    []Sent   `json:"sent"`
	// FailSend makes the next N `send` calls fail (and not be recorded).
	FailSend int `json:"fail_send"`
	// Reads counts `read --pending` invocations.
	Reads int `json:"reads"`
}

// Store is a file-backed State guarded by a lock file (safe across processes).
type Store struct{ Path string }

func (s Store) lock() (func(), error) {
	lp := s.Path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lp) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			os.Remove(lp) // stale lock from a killed process
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s Store) load() (State, error) {
	var st State
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("queue: parse %s: %w", s.Path, err)
	}
	return st, nil
}

// Update runs fn under the lock and persists the mutated state.
func (s Store) Update(fn func(*State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	ferr := fn(&st)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	return ferr
}

// Snapshot returns a copy of the current state.
func (s Store) Snapshot() (State, error) {
	var out State
	err := s.Update(func(st *State) error { out = *st; return nil })
	return out, err
}

// Enqueue appends inbound messages.
func (s Store) Enqueue(msgs ...Msg) error {
	return s.Update(func(st *State) error { st.Pending = append(st.Pending, msgs...); return nil })
}

// SetFailSend makes the next n sends fail.
func (s Store) SetFailSend(n int) error {
	return s.Update(func(st *State) error { st.FailSend = n; return nil })
}
