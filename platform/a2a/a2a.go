// Package a2a connects cc-connect to the Nibiru A2A message bus by polling the
// a2a CLI. See nibiru-labs/nibiru#836.
package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterPlatform("a2a", New)
}

const (
	defaultAllowFrom    = "grok,dot,claude-cloud,hermes"
	defaultPollInterval = 5 * time.Second
	defaultMaxChunk     = 6000
	doneTTL             = 10 * time.Minute
)

// replyContext is the replyCtx handed to Reply/Send.
type replyContext struct {
	ConversationID string
	MessageIDs     []string // inbound messages covered by this turn; acked once the turn succeeds
}

// Acker is implemented by L2 (ack.go).
type Acker interface {
	Track(sessionKey string, messageIDs []string)
	TurnSucceeded(sessionKey string)
	TurnFailed(sessionKey string, err error)
}

// TODO(836-a2a-ack): replace with the real Acker from ack.go once merged.
type noopAcker struct{}

func (noopAcker) Track(string, []string)   {}
func (noopAcker) TurnSucceeded(string)     {}
func (noopAcker) TurnFailed(string, error) {}

// Platform is the A2A chat platform.
type Platform struct {
	client   Client
	acker    Acker
	allow    map[string]bool
	interval time.Duration
	maxChunk int

	// pong answers liveness pings at platform level and reports whether the
	// message was consumed. TODO(836-a2a-ack): wire to pong.go; nil = not handled.
	pong func(ctx context.Context, m Inbound) bool

	mu       sync.Mutex
	inflight map[string]bool
	done     map[string]time.Time
	replies  map[string]int // successful Reply/Send count per first message id
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New builds the platform from config options: a2a_bin, allow_from,
// poll_interval (seconds or Go duration), max_chunk (bytes).
func New(opts map[string]any) (core.Platform, error) {
	bin, _ := opts["a2a_bin"].(string)
	p := newPlatform(newCLIClient(bin), noopAcker{}, opts)
	return p, nil
}

func newPlatform(c Client, a Acker, opts map[string]any) *Platform {
	allowFrom, _ := opts["allow_from"].(string)
	if strings.TrimSpace(allowFrom) == "" {
		allowFrom = defaultAllowFrom
	}
	allow := map[string]bool{}
	for _, s := range strings.Split(allowFrom, ",") {
		if s = strings.TrimSpace(s); s != "" {
			allow[strings.ToLower(s)] = true
		}
	}
	interval := defaultPollInterval
	switch v := opts["poll_interval"].(type) {
	case int:
		interval = time.Duration(v) * time.Second
	case int64:
		interval = time.Duration(v) * time.Second
	case float64:
		interval = time.Duration(v * float64(time.Second))
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		} else if n, err := strconv.Atoi(v); err == nil {
			interval = time.Duration(n) * time.Second
		}
	}
	if interval <= 0 {
		interval = defaultPollInterval
	}
	maxChunk := defaultMaxChunk
	switch v := opts["max_chunk"].(type) {
	case int:
		maxChunk = v
	case int64:
		maxChunk = int(v)
	case float64:
		maxChunk = int(v)
	}
	if maxChunk < 64 {
		maxChunk = defaultMaxChunk
	}
	return &Platform{
		client: c, acker: a, allow: allow, interval: interval, maxChunk: maxChunk,
		inflight: map[string]bool{}, done: map[string]time.Time{}, replies: map[string]int{},
	}
}

func (p *Platform) Name() string { return "a2a" }

// Start launches the polling goroutine; Stop makes it exit.
func (p *Platform) Start(handler core.MessageHandler) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return fmt.Errorf("a2a: already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.pollOnce(ctx, handler)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.pollOnce(ctx, handler)
			}
		}
	}()
	return nil
}

func (p *Platform) Stop() error {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *Platform) pollOnce(ctx context.Context, handler core.MessageHandler) {
	msgs, err := p.client.ReadPending(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("a2a: read pending failed", "error", err)
		}
		return
	}
	p.gcDone()
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		p.dispatch(ctx, handler, m)
	}
}

func (p *Platform) gcDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for id, t := range p.done {
		if now.Sub(t) > doneTTL {
			delete(p.done, id)
		}
	}
}

func (p *Platform) dispatch(ctx context.Context, handler core.MessageHandler, m Inbound) {
	if m.MessageID == "" {
		return
	}
	p.mu.Lock()
	if p.inflight[m.MessageID] {
		p.mu.Unlock()
		return
	}
	if _, ok := p.done[m.MessageID]; ok {
		p.mu.Unlock()
		return
	}
	p.inflight[m.MessageID] = true
	p.mu.Unlock()

	if p.pong != nil && p.pong(ctx, m) {
		p.release(m.MessageID, true)
		return
	}
	if !p.allow[strings.ToLower(m.SenderID)] {
		slog.Warn("a2a: dropping message from non-roster sender", "sender", m.SenderID, "message_id", m.MessageID)
		if err := p.client.Ack(ctx, m.MessageID); err != nil {
			slog.Warn("a2a: ack dropped message failed", "message_id", m.MessageID, "error", err)
			p.release(m.MessageID, false)
			return
		}
		p.release(m.MessageID, true)
		return
	}

	key := "a2a:" + m.ConversationID
	p.acker.Track(key, []string{m.MessageID})
	handler(p, &core.Message{
		SessionKey: key,
		Platform:   "a2a",
		MessageID:  m.MessageID,
		ChannelID:  m.ConversationID,
		UserID:     m.SenderID,
		UserName:   m.SenderID,
		Content:    m.Text,
		ReplyCtx:   &replyContext{ConversationID: m.ConversationID, MessageIDs: []string{m.MessageID}},
	})
}

// release clears the in-flight mark; completed=true remembers the id so a
// not-yet-visible ack cannot cause a re-dispatch.
func (p *Platform) release(id string, completed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, id)
	if completed {
		p.done[id] = time.Now()
	}
}

// Reply and Send only deliver; neither carries turn semantics. The engine
// reports the end of a turn through FinishTurn, which is what acks (or
// releases for redelivery) the inbound messages.
func (p *Platform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.send(ctx, replyCtx, content)
}

func (p *Platform) Send(ctx context.Context, replyCtx any, content string) error {
	return p.send(ctx, replyCtx, content)
}

func (p *Platform) send(ctx context.Context, replyCtx any, content string) error {
	rc, ok := replyCtx.(*replyContext)
	if !ok || rc == nil || len(rc.MessageIDs) == 0 {
		return fmt.Errorf("a2a: invalid reply context %T", replyCtx)
	}
	first := rc.MessageIDs[0]
	replyTo := rc.MessageIDs[len(rc.MessageIDs)-1]

	p.mu.Lock()
	n := p.replies[first]
	p.mu.Unlock()

	chunks := splitChunks(content, p.maxChunk)
	for i, text := range chunks {
		idem := fmt.Sprintf("%s-%d", first, i+1)
		if n > 0 {
			idem = fmt.Sprintf("%s-r%d-%d", first, n, i+1)
		}
		if err := p.client.SendReply(ctx, replyTo, idem, text); err != nil {
			return fmt.Errorf("a2a: send chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	p.mu.Lock()
	p.replies[first]++
	p.mu.Unlock()
	return nil
}

// FinishTurn implements core.TurnFinisher. The engine calls it exactly once
// per turn: ok means the turn completed and its reply was delivered, so the
// inbound messages are acked; otherwise they are released for redelivery.
func (p *Platform) FinishTurn(_ context.Context, replyCtx any, ok bool) {
	rc, isRC := replyCtx.(*replyContext)
	if !isRC || rc == nil || len(rc.MessageIDs) == 0 {
		return
	}
	sessionKey := "a2a:" + rc.ConversationID
	if ok {
		p.acker.TurnSucceeded(sessionKey)
	} else {
		p.acker.TurnFailed(sessionKey, fmt.Errorf("a2a: turn did not complete"))
	}
	p.finish(rc, ok)
}

func (p *Platform) finish(rc *replyContext, ok bool) {
	for _, id := range rc.MessageIDs {
		p.release(id, ok)
	}
}

// splitChunks splits content on rune boundaries into pieces of at most
// maxBytes bytes (including the "[i/n sha256:<12>] " prefix when n>1).
func splitChunks(content string, maxBytes int) []string {
	if len(content) <= maxBytes {
		return []string{content}
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])[:12]

	// The prefix width depends on n, which depends on the body size; iterate.
	n := 2
	for {
		prefixLen := len(fmt.Sprintf("[%d/%d sha256:%s] ", n, n, hash))
		body := maxBytes - prefixLen
		if body < utf8.UTFMax {
			body = utf8.UTFMax
		}
		parts := cut(content, body)
		if len(parts) <= n {
			out := make([]string, len(parts))
			for i, s := range parts {
				out[i] = fmt.Sprintf("[%d/%d sha256:%s] %s", i+1, len(parts), hash, s)
			}
			return out
		}
		n = len(parts)
	}
}

func cut(s string, max int) []string {
	var parts []string
	for len(s) > max {
		end := max
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		parts = append(parts, s[:end])
		s = s[end:]
	}
	if s != "" {
		parts = append(parts, s)
	}
	return parts
}
