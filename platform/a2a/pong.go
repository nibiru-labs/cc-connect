package a2a

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

// Liveness probe (nibiru#519, docs/a2a/pong-protocol.md): claude-cloud sends a
// nibiru.ping.v1 and expects a nibiru.pong.v1 reply-to, answered by code rather
// than by the model.
const (
	PingType   = "nibiru.ping.v1"
	PongType   = "nibiru.pong.v1"
	PingSender = "claude-cloud"

	// DefaultPongMaxAttempts matches the runner: give up (and ack) after 5 polling rounds.
	DefaultPongMaxAttempts = 5
)

var pingRequestIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// PingRequestID returns the request_id when text is a well-formed ping
// (JSON object, type nibiru.ping.v1, 16 lowercase-hex request_id).
func PingRequestID(text string) (string, bool) {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "{") {
		return "", false
	}
	var obj struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return "", false
	}
	if obj.Type != PingType || !pingRequestIDRe.MatchString(obj.RequestID) {
		return "", false
	}
	return obj.RequestID, true
}

// PongBody is the compact JSON body of the reply.
func PongBody(requestID string) string {
	b, _ := json.Marshal(struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
	}{PongType, requestID})
	return string(b)
}

// PongHandler answers pings at the platform layer, before the engine sees them.
type PongHandler struct {
	client      Client
	maxAttempts int

	mu       sync.Mutex
	attempts map[string]int // message_id -> failed pong sends
}

// NewPongHandler builds a handler; maxAttempts <= 0 uses DefaultPongMaxAttempts.
func NewPongHandler(c Client, maxAttempts int) *PongHandler {
	if maxAttempts <= 0 {
		maxAttempts = DefaultPongMaxAttempts
	}
	return &PongHandler{client: c, maxAttempts: maxAttempts, attempts: make(map[string]int)}
}

// Handle returns true when in is a ping that was consumed here (the caller must
// not forward it to the engine, nor ack it itself). Returns false for anything else.
//
// claude-cloud pings get a pong then an ack. A failed pong send leaves the
// message unacked so the next poll round retries; after maxAttempts failures it
// is acked anyway. Pings from other senders are only acked.
func (h *PongHandler) Handle(ctx context.Context, in Inbound) bool {
	rid, ok := PingRequestID(in.Text)
	if !ok {
		return false
	}
	if in.SenderID != PingSender {
		h.ack(ctx, in.MessageID)
		return true
	}
	err := h.client.SendReply(ctx, in.MessageID, "pong-"+rid, PongBody(rid))
	if err != nil {
		h.mu.Lock()
		h.attempts[in.MessageID]++
		n := h.attempts[in.MessageID]
		h.mu.Unlock()
		if n < h.maxAttempts {
			slog.Warn("a2a: pong send failed, will retry", "message_id", in.MessageID, "attempt", n, "error", err)
			return true
		}
		slog.Error("a2a: pong send failed, giving up", "message_id", in.MessageID, "attempts", n, "error", err)
	} else {
		slog.Info("a2a: pong sent", "message_id", in.MessageID)
	}
	h.mu.Lock()
	delete(h.attempts, in.MessageID)
	h.mu.Unlock()
	h.ack(ctx, in.MessageID)
	return true
}

func (h *PongHandler) ack(ctx context.Context, id string) {
	if err := h.client.Ack(ctx, id); err != nil {
		// Unacked ping reappears next poll; the pong key is idempotent.
		slog.Warn("a2a: ack ping failed", "message_id", id, "error", err)
	}
}
