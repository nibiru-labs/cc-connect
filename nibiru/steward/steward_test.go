package steward

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestShouldRotate(t *testing.T) {
	cases := []struct {
		name      string
		used, win int
		th        float64
		want, ok  bool
	}{
		{"below", 84, 100, 0.85, false, true},
		{"at", 85, 100, 0.85, true, true},
		{"above", 99, 100, 0.85, true, true},
		{"custom", 60, 100, 0.5, true, true},
		{"no window", 10, 0, 0.85, false, false},
		{"zero used", 0, 100, 0.85, false, true},
	}
	for _, c := range cases {
		s := SessionInfo{UsedTokens: c.used, ContextWindow: c.win}
		got, ok := ShouldRotate(s, c.th)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
	// reported percent takes precedence
	if got, ok := ShouldRotate(SessionInfo{Percent: 90}, 0.85); !got || !ok {
		t.Errorf("percent: got (%v,%v)", got, ok)
	}
}

func TestTruncateSummary(t *testing.T) {
	long := strings.Repeat("好", 3000)
	out := TruncateSummary(long, 2000)
	if n := utf8.RuneCountInString(out); n > 2000 {
		t.Fatalf("runes=%d > 2000", n)
	}
	if !utf8.ValidString(out) || !strings.HasSuffix(out, truncMark) {
		t.Fatalf("bad truncation: valid=%v", utf8.ValidString(out))
	}
	if got := TruncateSummary("short", 2000); got != "short" {
		t.Fatalf("short changed: %q", got)
	}
}

func TestBuildSummaryKeepsTailWithinLimit(t *testing.T) {
	var h []HistoryItem
	for i := 0; i < 200; i++ {
		h = append(h, HistoryItem{Role: "user", Content: strings.Repeat("x", 50)})
	}
	h = append(h, HistoryItem{Role: "assistant", Content: "LAST-MESSAGE"})
	s := BuildSummary(h, 500)
	if utf8.RuneCountInString(s) > 500 {
		t.Fatalf("too long: %d", utf8.RuneCountInString(s))
	}
	if !strings.Contains(s, "LAST-MESSAGE") {
		t.Fatalf("tail missing: %s", s)
	}
}

func TestHandoffPath(t *testing.T) {
	p := HandoffPath("/d", "a2a:conv/../x")
	if filepath.Dir(p) != filepath.Join("/d", "nibiru-handoff") {
		t.Fatalf("escaped dir: %s", p)
	}
	if !strings.HasSuffix(p, ".md") {
		t.Fatal(p)
	}
}

// fakeAPI emulates the (proposed) cc-connect management endpoints.
type fakeAPI struct {
	mu       sync.Mutex
	sessions []map[string]any
	history  []map[string]any
	newBody  map[string]any
	newCalls int
	token    string
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/api/v1/projects/p1/sessions", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"sessions": f.sessions}})
	}))
	mux.HandleFunc("/api/v1/projects/p1/sessions/s1", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"history": f.history}})
	}))
	mux.HandleFunc("/api/v1/projects/p1/sessions/new", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.newCalls++
		json.NewDecoder(r.Body).Decode(&f.newBody)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	return mux
}

func newFixture(t *testing.T, used, window int) (*Steward, *fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{
		token: "tok",
		sessions: []map[string]any{{
			"id": "s1", "session_key": "a2a:c1", "name": "n",
			"context_usage": map[string]any{"used_tokens": used, "context_window": window},
		}},
		history: []map[string]any{
			{"role": "user", "content": "do X"},
			{"role": "assistant", "content": "did X, next do Y"},
		},
	}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cl := NewHTTPClient(srv.URL, "tok", srv.Client())
	s := New(cl, Config{Projects: []string{"p1"}, DataDir: dir, Threshold: 0.85, MaxSummaryRunes: 2000, Cooldown: time.Hour})
	return s, f, dir
}

func TestRunOnceRotatesAndInjects(t *testing.T) {
	s, f, dir := newFixture(t, 90, 100)
	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].Rotated {
		t.Fatalf("res=%+v", res)
	}
	b, err := os.ReadFile(HandoffPath(dir, "a2a:c1"))
	if err != nil || !strings.Contains(string(b), "did X, next do Y") {
		t.Fatalf("handoff file: %v %q", err, b)
	}
	if f.newCalls != 1 || f.newBody["session_key"] != "a2a:c1" {
		t.Fatalf("new call: %+v", f.newBody)
	}
	if ctxText, _ := f.newBody["context"].(string); !strings.Contains(ctxText, "did X, next do Y") {
		t.Fatalf("summary not injected: %q", ctxText)
	}
}

func TestRunOnceBelowThresholdAndCooldown(t *testing.T) {
	s, f, _ := newFixture(t, 50, 100)
	res, _ := s.RunOnce(context.Background())
	if len(res) != 0 || f.newCalls != 0 {
		t.Fatalf("rotated below threshold: %+v", res)
	}
	f.sessions[0]["context_usage"] = map[string]any{"used_tokens": 95, "context_window": 100}
	s.RunOnce(context.Background())
	s.RunOnce(context.Background()) // within cooldown
	if f.newCalls != 1 {
		t.Fatalf("newCalls=%d want 1 (cooldown)", f.newCalls)
	}
}

func TestRunOnceNoUsageReportedSkips(t *testing.T) {
	s, f, _ := newFixture(t, 0, 0)
	delete(f.sessions[0], "context_usage")
	res, err := s.RunOnce(context.Background())
	if err != nil || len(res) != 0 || f.newCalls != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestClientAuthFailure(t *testing.T) {
	f := &fakeAPI{token: "right"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cl := NewHTTPClient(srv.URL, "wrong", srv.Client())
	if _, err := cl.ListSessions(context.Background(), "p1"); err == nil {
		t.Fatal("expected error")
	} else if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("token leaked: %v", err)
	}
}
