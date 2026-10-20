package a2a

import (
	"regexp"
	"strings"
	"sync"
)

// nibiru#1346: two A2A peers kept answering each other's empty replies
// ("(空响应)" <-> "(空响应)") for hundreds of turns in one conversation,
// burning tokens on both sides. The engine substitutes an i18n placeholder for
// an empty agent reply (core/engine.go, MsgEmptyResponse). On A2A that
// placeholder is itself a message, so it wakes the peer's model, which replies
// empty again, and the loop keeps going.
//
// Two guards live here:
//
//  1. send() never delivers an empty / placeholder / NO_REPLY reply. The turn is
//     still settled, so the inbound message is acked and nothing is redelivered.
//  2. a per-conversation breaker: after DefaultEmptyExchangeLimit consecutive
//     empty exchanges, an empty inbound is acked without running a model turn
//     at all. An exchange that carries real content on both sides resets the
//     counter.

// DefaultEmptyExchangeLimit is the breaker threshold: the number of
// consecutive empty exchanges a conversation may have before the platform
// stops waking the model for it.
const DefaultEmptyExchangeLimit = 3

// emptyReplyPlaceholders are the core.MsgEmptyResponse renderings, one per
// shipped language. They are matched exactly: user text that merely contains
// one of them is ordinary content.
var emptyReplyPlaceholders = map[string]bool{
	"(empty response)":  true,
	"(空响应)":             true,
	"(空回應)":             true,
	"（空のレスポンス）":         true,
	"(respuesta vacía)": true,
}

// noReplyMarker is the conventional "do not answer" marker.
var noReplyMarker = regexp.MustCompile(`(?i)^\s*NO_REPLY\s*$`)

// isEmptyReply reports whether a reply carries no content worth delivering or
// answering: blank, a bare NO_REPLY marker, or the engine's empty-response
// placeholder.
func isEmptyReply(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || noReplyMarker.MatchString(t) {
		return true
	}
	return emptyReplyPlaceholders[t]
}

// emptyExchangeBreaker counts consecutive empty exchanges per conversation.
type emptyExchangeBreaker struct {
	limit int

	mu          sync.Mutex
	consecutive map[string]int
}

func newEmptyExchangeBreaker(limit int) *emptyExchangeBreaker {
	if limit <= 0 {
		limit = DefaultEmptyExchangeLimit
	}
	return &emptyExchangeBreaker{limit: limit, consecutive: map[string]int{}}
}

// tripped reports whether the conversation already used up its empty-exchange
// budget.
func (b *emptyExchangeBreaker) tripped(conversation string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive[conversation] >= b.limit
}

// record accounts for one finished exchange and returns the new consecutive
// count. An exchange counts as empty when either side carried no real content;
// a real exchange resets the conversation counter.
func (b *emptyExchangeBreaker) record(conversation string, empty bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if empty {
		b.consecutive[conversation]++
	} else {
		delete(b.consecutive, conversation)
	}
	return b.consecutive[conversation]
}

// count returns the consecutive empty exchanges of a conversation.
func (b *emptyExchangeBreaker) count(conversation string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive[conversation]
}
