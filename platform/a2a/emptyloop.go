package a2a

import (
	"regexp"
	"strings"
	"sync"
)

// nibiru#1346: two A2A peers answering each other's empty replies looped for
// hundreds of turns ("(空响应)" <-> "(空响应)"). The engine substitutes an i18n
// placeholder for an empty agent reply; on A2A that placeholder is itself a
// message that wakes the peer's model, which again replies empty, and so on.
//
// Two guards live here:
//  1. send never delivers an empty / placeholder / NO_REPLY reply (it is
//     treated as delivered so the inbound message is still acked).
//  2. a per-conversation breaker: after DefaultEmptyExchangeLimit consecutive
//     empty exchanges (inbound empty, or the turn produced no real reply), an
//     inbound empty/placeholder message is acked without running the model.
//     Any exchange with real content on both sides resets the counter.

// DefaultEmptyExchangeLimit is the breaker threshold (consecutive empty exchanges).
const DefaultEmptyExchangeLimit = 3

// emptyPlaceholders are core.MsgEmptyResponse in every shipped language.
var emptyPlaceholders = map[string]bool{
	"(empty response)":  true,
	"(空响应)":             true,
	"(空回應)":             true,
	"（空のレスポンス）":         true,
	"(respuesta vacía)": true,
	"（空响应）":             true,
}

var noReplyRe = regexp.MustCompile(`(?i)^\s*NO_REPLY\s*$`)

// isEmptyReply reports whether raw text carries no content worth delivering or
// answering: blank, a bare NO_REPLY marker, or the engine's exact placeholder.
// Footer-decorated legacy text is intentionally ambiguous on inbound A2A: the
// transport provides no trusted body/footer metadata to distinguish it from
// substantive text, so the footer-aware send path must prevent new instances.
func isEmptyReply(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || noReplyRe.MatchString(t) {
		return true
	}
	return emptyPlaceholders[t]
}

// emptyBreaker tracks consecutive empty exchanges per conversation.
type emptyBreaker struct {
	limit int

	mu           sync.Mutex
	consecutive  map[string]int  // conversation id -> consecutive empty exchanges
	inboundEmpty map[string]bool // first message id of a running turn -> inbound was empty
	realSent     map[string]bool // first message id -> a non-empty reply was delivered
}

func newEmptyBreaker(limit int) *emptyBreaker {
	if limit <= 0 {
		limit = DefaultEmptyExchangeLimit
	}
	return &emptyBreaker{
		limit:        limit,
		consecutive:  map[string]int{},
		inboundEmpty: map[string]bool{},
		realSent:     map[string]bool{},
	}
}

// shouldDrop reports whether an inbound message must be acked without a model
// turn: it is empty and the conversation already hit the breaker limit.
func (b *emptyBreaker) shouldDrop(conv, text string) bool {
	if !isEmptyReply(text) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive[conv] >= b.limit
}

// begin records the inbound side of a turn.
func (b *emptyBreaker) begin(firstID, text string) {
	b.mu.Lock()
	b.inboundEmpty[firstID] = isEmptyReply(text)
	b.mu.Unlock()
}

// discard removes a turn that was settled by a platform control policy rather
// than by an assistant response. It must not change the empty-exchange count.
func (b *emptyBreaker) discard(firstID string) {
	b.mu.Lock()
	delete(b.inboundEmpty, firstID)
	delete(b.realSent, firstID)
	b.mu.Unlock()
}

// delivered records that a real (non-empty) reply was sent for the turn.
func (b *emptyBreaker) delivered(firstID string) {
	b.mu.Lock()
	b.realSent[firstID] = true
	b.mu.Unlock()
}

// finish closes the exchange and updates the conversation counter. It returns
// the new consecutive count.
func (b *emptyBreaker) finish(conv, firstID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	empty := b.inboundEmpty[firstID] || !b.realSent[firstID]
	delete(b.inboundEmpty, firstID)
	delete(b.realSent, firstID)
	if empty {
		b.consecutive[conv]++
	} else {
		delete(b.consecutive, conv)
	}
	return b.consecutive[conv]
}

// count returns the current consecutive empty exchanges for conv (tests/logs).
func (b *emptyBreaker) count(conv string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive[conv]
}
