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
	ConversationID    string
	MessageIDs        []string // inbound messages covered by this turn; acked once the turn succeeds
	rosterTransferKey string   // non-empty only for a completely validated roster transfer
}

// Acker is implemented by L2 (ack.go).
// Settlement is by the turn's own message IDs: a turn must never settle a
// later message that is merely queued behind it on the same session.
type Acker interface {
	Admit(messageID string) bool
	Track(sessionKey string, messageIDs []string)
	TurnSucceededIDs(messageIDs []string)
	TurnFailedIDs(sessionKey string, messageIDs []string, err error)
}

var _ Acker = (*DefaultAcker)(nil)

// Platform is the A2A chat platform.
type Platform struct {
	client   Client
	acker    Acker
	allow    map[string]bool
	roster   *rosterReceiver
	interval time.Duration
	maxChunk int
	empty    *emptyBreaker // nibiru#1346 empty-reply loop guard

	// pong answers liveness pings at platform level and reports whether the
	// message was consumed. TODO(836-a2a-ack): wire to pong.go; nil = not handled.
	pong func(ctx context.Context, m Inbound) (consumed, settled bool)

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
	c := newCLIClient(bin)
	p := newPlatform(c, NewAcker(c, AckerConfig{}), opts)
	if p.roster != nil && p.roster.configErr != nil {
		return nil, fmt.Errorf("a2a: invalid roster_issuer configuration: %w", p.roster.configErr)
	}
	p.pong = NewPongHandler(c, 0).HandleSettled
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
		client: c, acker: a, allow: allow, roster: newRosterReceiver(opts, allow), interval: interval, maxChunk: maxChunk,
		inflight: map[string]bool{}, done: map[string]time.Time{}, replies: map[string]int{},
		empty: newEmptyBreaker(emptyLimitOpt(opts["empty_reply_limit"])),
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

	if !p.acker.Admit(m.MessageID) {
		p.release(m.MessageID, false)
		return
	}

	if p.pong != nil {
		if consumed, settled := p.pong(ctx, m); consumed {
			// Only a terminal (acked) ping is remembered as done; a failed pong
			// send or ack must stay retryable on the next poll round.
			p.release(m.MessageID, settled)
			return
		}
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
	if p.roster != nil && p.roster.isTransferMessage(m.Text) {
		p.dispatchRoster(ctx, handler, m)
		return
	}

	p.dispatchMessage(handler, m, m.Text, "")
}

func (p *Platform) dispatchRoster(ctx context.Context, handler core.MessageHandler, m Inbound) {
	result := p.roster.accept(m)
	logRosterResult(m, result)
	if result.retryable {
		slog.Warn("a2a: roster state unavailable; message remains retryable",
			"message_id", m.MessageID, "reason", result.reason)
		p.release(m.MessageID, false)
		return
	}
	if result.state != rosterReceiveReady {
		// Partial pieces are acknowledged only after durable persistence. Invalid
		// and duplicate envelopes are terminal transport inputs and never reach
		// the model path.
		p.acker.TurnSucceededIDs([]string{m.MessageID})
		p.release(m.MessageID, true)
		return
	}
	p.dispatchMessage(handler, m, result.payload, result.key)
}

func (p *Platform) dispatchMessage(handler core.MessageHandler, m Inbound, content, rosterTransferKey string) {
	if rosterTransferKey == "" && p.empty.shouldDrop(m.ConversationID, content) {
		// Keep breaker settlement on the Acker path so per-ID retry/ACK state
		// remains consistent with every other accepted inbound message.
		slog.Warn("a2a: empty-reply breaker open, settling without model dispatch",
			"conversation", m.ConversationID, "message_id", m.MessageID, "sender", m.SenderID,
			"consecutive_empty", p.empty.count(m.ConversationID))
		p.acker.TurnSucceededIDs([]string{m.MessageID})
		p.release(m.MessageID, true)
		return
	}
	key := "a2a:" + m.ConversationID
	if rosterTransferKey == "" {
		p.empty.begin(m.MessageID, content)
	}
	p.acker.Track(key, []string{m.MessageID})
	handler(p, &core.Message{
		SessionKey: key,
		Platform:   "a2a",
		MessageID:  m.MessageID,
		ChannelID:  m.ConversationID,
		UserID:     m.SenderID,
		UserName:   m.SenderID,
		Content:    content,
		ReplyCtx:   &replyContext{ConversationID: m.ConversationID, MessageIDs: []string{m.MessageID}, rosterTransferKey: rosterTransferKey},
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

// SendWithStatusFooter implements core.StatusFooterSender. Classify the raw
// body before composing the footer so metadata cannot turn an empty reply into
// a message that wakes an A2A peer.
func (p *Platform) SendWithStatusFooter(ctx context.Context, replyCtx any, content, footer string) error {
	if isEmptyReply(content) {
		return p.send(ctx, replyCtx, content)
	}
	return p.send(ctx, replyCtx, appendInlineStatusFooter(content, footer))
}

func appendInlineStatusFooter(content, footer string) string {
	if footer == "" {
		return content
	}
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return "*" + footer + "*"
	}
	return content + "\n\n*" + footer + "*"
}

func (p *Platform) send(ctx context.Context, replyCtx any, content string) error {
	rc, ok := replyCtx.(*replyContext)
	if !ok || rc == nil || len(rc.MessageIDs) == 0 {
		return fmt.Errorf("a2a: invalid reply context %T", replyCtx)
	}
	first := rc.MessageIDs[0]
	replyTo := rc.MessageIDs[len(rc.MessageIDs)-1]
	if isEmptyReply(content) {
		slog.Info("a2a: suppressed empty reply", "conversation", rc.ConversationID, "reply_to", replyTo)
		return nil
	}

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
	if rc.rosterTransferKey == "" {
		p.empty.delivered(first)
	}
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
	if rc.rosterTransferKey == "" {
		if n := p.empty.finish(rc.ConversationID, rc.MessageIDs[0]); n >= p.empty.limit {
			slog.Warn("a2a: conversation hit empty-reply limit; further empty messages will be settled without model dispatch",
				"conversation", rc.ConversationID, "consecutive_empty", n)
		}
	}
	if ok {
		if rc.rosterTransferKey != "" && p.roster != nil {
			if err := p.roster.markDelivered(rc.rosterTransferKey, rc.MessageIDs[0]); err != nil {
				p.acker.TurnFailedIDs(sessionKey, rc.MessageIDs, fmt.Errorf("a2a: persist roster completion: %w", err))
				p.finish(rc, false)
				return
			}
		}
		p.acker.TurnSucceededIDs(rc.MessageIDs)
	} else {
		p.acker.TurnFailedIDs(sessionKey, rc.MessageIDs, fmt.Errorf("a2a: turn did not complete"))
	}
	p.finish(rc, ok)
}

// SuppressSystemResponse implements core.SystemResponseSuppressor. A2A peers
// must not receive an engine rate-limit notice as a new conversation prompt.
// A ready roster payload has not reached the model, so it remains retryable.
func (p *Platform) SuppressSystemResponse(_ context.Context, replyCtx any, kind core.SystemResponseKind) bool {
	if kind != core.SystemResponseRateLimited {
		return false
	}
	rc, ok := replyCtx.(*replyContext)
	if !ok || rc == nil || len(rc.MessageIDs) == 0 {
		return false
	}
	p.empty.discard(rc.MessageIDs[0])
	if rc.rosterTransferKey != "" {
		p.acker.TurnFailedIDs("a2a:"+rc.ConversationID, rc.MessageIDs, fmt.Errorf("a2a: roster turn rate limited before model dispatch"))
		p.finish(rc, false)
		return true
	}
	p.acker.TurnSucceededIDs(rc.MessageIDs)
	p.finish(rc, true)
	slog.Info("a2a: suppressed system response and settled inbound turn",
		"kind", "rate_limited", "conversation", rc.ConversationID, "message_ids", rc.MessageIDs)
	return true
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

// emptyLimitOpt parses the optional empty_reply_limit option (0 = default).
func emptyLimitOpt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}
