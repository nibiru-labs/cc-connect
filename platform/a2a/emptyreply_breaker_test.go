package a2a

import "testing"

// nibiru#1346: unit tests for the empty-reply detection and the
// per-conversation breaker.

func TestIsEmptyReply(t *testing.T) {
	empty := []string{
		"",
		"   ",
		"\n\t",
		"NO_REPLY",
		"no_reply",
		"  NO_REPLY  ",
		"(empty response)",
		"(空响应)",
		"(空回應)",
		"（空のレスポンス）",
		"(respuesta vacía)",
	}
	for _, text := range empty {
		if !isEmptyReply(text) {
			t.Errorf("isEmptyReply(%q) = false, want true", text)
		}
	}
	content := []string{
		"hello",
		"the answer is (空响应) in my text",
		"NO_REPLY is a word in a sentence",
		"(空响应x)",
	}
	for _, text := range content {
		if isEmptyReply(text) {
			t.Errorf("isEmptyReply(%q) = true, want false", text)
		}
	}
}

func TestEmptyExchangeBreaker(t *testing.T) {
	b := newEmptyExchangeBreaker(2)
	if b.tripped("ac-1") {
		t.Fatalf("breaker tripped before any exchange")
	}
	if got := b.record("ac-1", true); got != 1 {
		t.Fatalf("record = %d, want 1", got)
	}
	if b.tripped("ac-1") {
		t.Fatalf("breaker tripped after 1 of 2 empty exchanges")
	}
	b.record("ac-1", true)
	if !b.tripped("ac-1") {
		t.Fatalf("breaker not tripped after 2 empty exchanges")
	}
	if b.tripped("ac-2") {
		t.Fatalf("breaker tripped for another conversation")
	}
	// A real exchange resets the counter.
	b.record("ac-1", false)
	if b.tripped("ac-1") {
		t.Fatalf("breaker still tripped after a real exchange")
	}
	if got := b.count("ac-1"); got != 0 {
		t.Fatalf("count = %d, want 0 after a real exchange", got)
	}
}
