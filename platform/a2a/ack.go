package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultAckMaxAttempts is how many failed turns a message gets before it is
// reported and acked so it cannot loop forever.
const DefaultAckMaxAttempts = 5

// AckerConfig tunes DefaultAcker. Zero values select defaults.
type AckerConfig struct {
	MaxAttempts int                                                     // failed turns before alert+ack (default 5)
	Backoff     func(attempt int) time.Duration                         // delay before redelivery after the n-th failure (default 5s·2^(n-1), cap 5m)
	Now         func() time.Time                                        // test hook
	Alert       func(sessionKey string, messageIDs []string, err error) // called once when giving up
	AckTimeout  time.Duration                                           // per Ack call (default 30s)
	// DisableAckRetry turns off the background timer for failed acks (tests call RetryAcks).
	DisableAckRetry bool
}

type msgState struct {
	attempts int
	inFlight bool
	notUntil time.Time
}

// DefaultAcker implements Acker. Safe for concurrent use.
//
// Contract with the platform poll loop: before handing a message from
// `read --pending` to the engine, call Admit(id); a false return means the
// message is mid-turn, backing off, or waiting for an ack retry, so skip it.
// Then Track(sessionKey, ids).
type DefaultAcker struct {
	client Client
	cfg    AckerConfig

	mu       sync.Mutex
	sessions map[string][]string // sessionKey -> message ids of the running turn
	msgs     map[string]*msgState
	unacked  map[string]struct{} // turn finished, Ack call failed; retry ack only (never rerun the model)
	closed   bool
	timer    *time.Timer
}

var _ Acker = (*DefaultAcker)(nil)

// NewAcker builds a DefaultAcker.
func NewAcker(c Client, cfg AckerConfig) *DefaultAcker {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultAckMaxAttempts
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = 30 * time.Second
	}
	return &DefaultAcker{
		client:   c,
		cfg:      cfg,
		sessions: make(map[string][]string),
		msgs:     make(map[string]*msgState),
		unacked:  make(map[string]struct{}),
	}
}

func defaultBackoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// Admit reports whether a message that appeared in `read --pending` may be
// delivered to the engine now.
func (a *DefaultAcker) Admit(messageID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, waiting := a.unacked[messageID]; waiting {
		return false
	}
	st := a.msgs[messageID]
	if st == nil {
		return true
	}
	if st.inFlight {
		return false
	}
	return !a.cfg.Now().Before(st.notUntil)
}

// Track registers messages handed to the engine for sessionKey.
func (a *DefaultAcker) Track(sessionKey string, messageIDs []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.sessions[sessionKey]
	for _, id := range messageIDs {
		if !contains(cur, id) {
			cur = append(cur, id)
		}
		st := a.msgs[id]
		if st == nil {
			st = &msgState{}
			a.msgs[id] = st
		}
		st.inFlight = true
	}
	a.sessions[sessionKey] = cur
}

// TurnSucceeded acks every message tracked for sessionKey.
func (a *DefaultAcker) TurnSucceeded(sessionKey string) {
	a.mu.Lock()
	ids := a.sessions[sessionKey]
	delete(a.sessions, sessionKey)
	a.mu.Unlock()
	for _, id := range ids {
		a.ackOne(context.Background(), id)
	}
}

// TurnFailed leaves the tracked messages unacked so they reappear on a later
// poll after a backoff. A message that has failed MaxAttempts times is reported
// via Alert and acked.
func (a *DefaultAcker) TurnFailed(sessionKey string, err error) {
	a.mu.Lock()
	ids := a.sessions[sessionKey]
	delete(a.sessions, sessionKey)
	var giveUp []string
	for _, id := range ids {
		st := a.msgs[id]
		if st == nil {
			st = &msgState{}
			a.msgs[id] = st
		}
		st.attempts++
		st.inFlight = false
		if st.attempts >= a.cfg.MaxAttempts {
			giveUp = append(giveUp, id)
			continue
		}
		st.notUntil = a.cfg.Now().Add(a.cfg.Backoff(st.attempts))
		slog.Warn("a2a: turn failed, message will be redelivered", "session_key", sessionKey,
			"message_id", id, "attempt", st.attempts, "error", err)
	}
	a.mu.Unlock()
	if len(giveUp) == 0 {
		return
	}
	slog.Error("a2a: giving up on message after repeated failures", "session_key", sessionKey,
		"message_ids", giveUp, "attempts", a.cfg.MaxAttempts, "error", err)
	if a.cfg.Alert != nil {
		a.cfg.Alert(sessionKey, giveUp, fmt.Errorf("a2a: %d failed turns: %w", a.cfg.MaxAttempts, err))
	}
	for _, id := range giveUp {
		a.ackOne(context.Background(), id)
	}
}

func (a *DefaultAcker) ackOne(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.AckTimeout)
	defer cancel()
	if err := a.client.Ack(ctx, id); err != nil {
		slog.Warn("a2a: ack failed, will retry", "message_id", id, "error", err)
		a.mu.Lock()
		a.unacked[id] = struct{}{}
		if st := a.msgs[id]; st != nil {
			st.inFlight = false
		}
		a.scheduleRetryLocked()
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	delete(a.msgs, id)
	delete(a.unacked, id)
	a.mu.Unlock()
}

// RetryAcks retries acks that failed earlier.
func (a *DefaultAcker) RetryAcks(ctx context.Context) {
	a.mu.Lock()
	ids := make([]string, 0, len(a.unacked))
	for id := range a.unacked {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.ackOne(ctx, id)
	}
}

func (a *DefaultAcker) scheduleRetryLocked() {
	if a.cfg.DisableAckRetry || a.closed || a.timer != nil {
		return
	}
	a.timer = time.AfterFunc(a.cfg.Backoff(1), func() {
		a.mu.Lock()
		a.timer = nil
		closed := a.closed
		a.mu.Unlock()
		if !closed {
			a.RetryAcks(context.Background())
		}
	})
}

// Close stops the background ack-retry timer.
func (a *DefaultAcker) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
