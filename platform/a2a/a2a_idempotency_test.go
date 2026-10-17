package a2a

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Key format: "<first inbound id>-h<12 hex chars>-<1-based chunk index>",
// e.g. "am-abc-h1a2b3c4d5e6f-1".
var idemKeyRe = regexp.MustCompile(`^am-abc-h[0-9a-f]{12}-(\d+)$`)

// The key is derived from content only: re-sending identical content (e.g. a
// reply replayed after a restart) yields the same key so the bus can dedupe it.
func TestSendIdempotencyKey_SameContentSameKeyAcrossRestarts(t *testing.T) {
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"am-abc"}}

	c1 := &l1Client{}
	p1 := newPlatform(c1, &l1Acker{}, nil)
	if err := p1.Reply(context.Background(), rc, "hello"); err != nil {
		t.Fatal(err)
	}

	// A brand-new Platform (fresh process, empty in-memory state) sending the
	// same content for the same inbound message.
	c2 := &l1Client{}
	p2 := newPlatform(c2, &l1Acker{}, nil)
	if err := p2.Reply(context.Background(), rc, "hello"); err != nil {
		t.Fatal(err)
	}

	if len(c1.sent) != 1 || len(c2.sent) != 1 {
		t.Fatalf("sent counts = %d, %d, want 1 each", len(c1.sent), len(c2.sent))
	}
	if c1.sent[0].key != c2.sent[0].key {
		t.Fatalf("keys differ after restart: %q vs %q", c1.sent[0].key, c2.sent[0].key)
	}
	if !idemKeyRe.MatchString(c1.sent[0].key) {
		t.Fatalf("key %q does not match %s", c1.sent[0].key, idemKeyRe)
	}
	if !strings.HasPrefix(c1.sent[0].key, "am-abc-h") || !strings.HasSuffix(c1.sent[0].key, "-1") {
		t.Fatalf("key %q, want am-abc-h<hash>-1", c1.sent[0].key)
	}
}

// Two different replies to the same inbound message must not collapse onto one
// key (the old in-memory reply counter made this depend on process-local state).
func TestSendIdempotencyKey_DifferentContentDifferentKey(t *testing.T) {
	c := &l1Client{}
	p := newPlatform(c, &l1Acker{}, nil)
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"am-abc"}}
	for _, text := range []string{"first answer", "second answer"} {
		if err := p.Reply(context.Background(), rc, text); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sent) != 2 {
		t.Fatalf("sent = %d, want 2", len(c.sent))
	}
	if c.sent[0].key == c.sent[1].key {
		t.Fatalf("different content produced the same key %q", c.sent[0].key)
	}
	// Same text again later in the session returns to the first key.
	if err := p.Send(context.Background(), rc, "first answer"); err != nil {
		t.Fatal(err)
	}
	if c.sent[2].key != c.sent[0].key {
		t.Fatalf("identical content must reuse key %q, got %q", c.sent[0].key, c.sent[2].key)
	}
	if c.sent[2].key == c.sent[1].key {
		t.Fatal("third reply collapsed onto the second reply's key")
	}
}

// Every chunk of a split reply gets its own key: same hash scheme, index 1..n.
func TestSendIdempotencyKey_ChunkIndexPerFragment(t *testing.T) {
	c := &l1Client{}
	p := newPlatform(c, &l1Acker{}, map[string]any{"max_chunk": 200})
	text := strings.Repeat("你好世界", 200) // 2400 bytes -> multiple chunks
	rc := &replyContext{ConversationID: "c1", MessageIDs: []string{"am-abc"}}
	if err := p.Reply(context.Background(), rc, text); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) < 3 {
		t.Fatalf("chunks = %d, want at least 3", len(c.sent))
	}
	seen := map[string]bool{}
	for i, s := range c.sent {
		want := idempotencyKey("am-abc", s.text, i+1)
		if s.key != want {
			t.Fatalf("chunk %d key = %q, want %q", i, s.key, want)
		}
		m := idemKeyRe.FindStringSubmatch(s.key)
		if m == nil {
			t.Fatalf("chunk %d key %q malformed", i, s.key)
		}
		if m[1] != strconv.Itoa(i+1) {
			t.Fatalf("chunk %d key %q has index %s", i, s.key, m[1])
		}
		if seen[s.key] {
			t.Fatalf("duplicate key across chunks: %q", s.key)
		}
		seen[s.key] = true
	}
	// Replaying the same long reply (e.g. after a restart) reproduces the keys.
	c2 := &l1Client{}
	p2 := newPlatform(c2, &l1Acker{}, map[string]any{"max_chunk": 200})
	if err := p2.Reply(context.Background(), rc, text); err != nil {
		t.Fatal(err)
	}
	if len(c2.sent) != len(c.sent) {
		t.Fatalf("replay chunks = %d, want %d", len(c2.sent), len(c.sent))
	}
	for i := range c.sent {
		if c2.sent[i].key != c.sent[i].key {
			t.Fatalf("chunk %d key not stable across replay: %q vs %q", i, c.sent[i].key, c2.sent[i].key)
		}
	}
}

// idempotencyKey itself: content-derived, index-sensitive, 12 hex chars.
func TestIdempotencyKey_Shape(t *testing.T) {
	k := idempotencyKey("am-abc", "hello", 1)
	if k != idempotencyKey("am-abc", "hello", 1) {
		t.Fatal("key must be a pure function of (firstID, chunk, index)")
	}
	if k == idempotencyKey("am-abc", "hello", 2) {
		t.Fatal("chunk index must change the key")
	}
	if k == idempotencyKey("am-abc", "hellp", 1) {
		t.Fatal("chunk text must change the key")
	}
	if k == idempotencyKey("am-xyz", "hello", 1) {
		t.Fatal("first inbound id must change the key")
	}
	parts := strings.Split(k, "-")
	// "am","abc","h<hash>","1" -> the hash segment carries 1 leading 'h' + 12 hex.
	if len(parts) != 4 || len(parts[2]) != 13 || !strings.HasPrefix(parts[2], "h") {
		t.Fatalf("key %q has unexpected shape", k)
	}
	for _, r := range parts[2][1:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("key %q is not lowercase hex", k)
		}
	}
}
