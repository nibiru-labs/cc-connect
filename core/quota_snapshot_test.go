package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type quotaStub struct {
	stubAgentSession
	calls int
}

func (s *quotaStub) CachedQuotaSnapshot() QuotaSnapshot {
	s.calls++
	return QuotaSnapshot{Source: "codex app-server received cache", AsOf: time.Now(), RateLimits: map[string]QuotaWindow{"window_0": {UsedPercent: 81}}}
}
func TestQuotaEndpointSingleSourceUnknownAndMethods(t *testing.T) {
	q := &quotaStub{}
	e := &Engine{interactiveStates: map[string]*interactiveState{"PRIVATE_KEY": {agentSession: q}, "alias": {agentSession: q}}}
	a := &APIServer{engines: map[string]*Engine{"PRIVATE_PROJECT": e}}
	check := func(want int) {
		rec := httptest.NewRecorder()
		a.handleQuota(rec, httptest.NewRequest("GET", "/quota", nil))
		var got QuotaSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.RateLimits) != want {
			t.Fatal("ambiguous/unknown quota accepted", rec.Body)
		}
	}
	check(1)
	if q.calls != 1 {
		t.Fatal("alias resampled")
	}
	e.interactiveStates["second"] = &interactiveState{agentSession: &quotaStub{}}
	check(0)
	e.interactiveStates = map[string]*interactiveState{"unsupported": {agentSession: &stubAgentSession{}}}
	check(0)
	e.interactiveStates = map[string]*interactiveState{}
	check(0)
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		rec := httptest.NewRecorder()
		a.handleQuota(rec, httptest.NewRequest(method, "/quota", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Fatal("write accepted")
		}
	}
}
