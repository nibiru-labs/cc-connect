package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuotaFailedRefreshCannotReturnCacheAsSuccess(t *testing.T) {
	s := &appServerSession{}
	s.storeUsage(&core.UsageReport{Provider: "codex", Buckets: []core.UsageBucket{{Allowed: true, Windows: []core.UsageWindow{{UsedPercent: 10}}}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := s.GetUsage(ctx)
	if err == nil {
		t.Fatal("refresh failure returned cached quota as successful current observation")
	}
	if report == nil {
		t.Fatal("diagnostic cache lost")
	}
}

func quotaEvent(s *appServerSession, used int) {
	s.handleNotification("account/rateLimits/updated", json.RawMessage(fmt.Sprintf(`{"rateLimits":{"limitId":"codex","planType":"PRIVATE_PLAN","primary":{"usedPercent":%d,"windowDurationMins":300}}}`, used)))
}
func TestQuotaCacheReceiptTimeFailureAndRecovery(t *testing.T) {
	s := &appServerSession{}
	s.alive.Store(true)
	before := time.Now()
	quotaEvent(s, 10)
	report := s.cachedUsage()
	if report.ObservedAt.Before(before) || report.ObservedAt.After(time.Now()) || report.Stale {
		t.Fatal("missing local receipt timestamp")
	}
	first := s.CachedQuotaSnapshot()
	second := s.CachedQuotaSnapshot()
	if !first.AsOf.Equal(second.AsOf) || len(first.RateLimits) != 1 {
		t.Fatal("read restamped or lost cache")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.GetUsage(ctx)
	if err == nil || !s.cachedUsage().Stale || len(s.CachedQuotaSnapshot().RateLimits) != 0 || !s.CachedQuotaSnapshot().AsOf.Equal(first.AsOf) {
		t.Fatal("failed refresh leaked a fresh cache")
	}
	quotaEvent(s, 81)
	if s.cachedUsage().Stale || len(s.CachedQuotaSnapshot().RateLimits) != 1 || s.cachedUsage().ObservedAt.Before(first.AsOf) {
		t.Fatal("new event did not restore observation")
	}
	b, _ := json.Marshal(s.CachedQuotaSnapshot())
	for _, bad := range []string{"PRIVATE_PLAN", "AccountID", "thread", "credits", "prompt"} {
		if strings.Contains(string(b), bad) {
			t.Fatal("private field leaked")
		}
	}
}
func TestQuotaUnknownCacheStates(t *testing.T) {
	for _, kind := range []string{"missing", "closed", "old", "future", "no-time", "invalid-window", "invalid-provider"} {
		t.Run(kind, func(t *testing.T) {
			s := &appServerSession{}
			s.alive.Store(true)
			if kind != "missing" {
				quotaEvent(s, 10)
			}
			if kind == "closed" {
				s.alive.Store(false)
			}
			s.runtimeMu.Lock()
			if s.usage != nil {
				switch kind {
				case "old":
					s.usage.ObservedAt = time.Now().Add(-121 * time.Second)
				case "future":
					s.usage.ObservedAt = time.Now().Add(3 * time.Second)
				case "no-time":
					s.usage.ObservedAt = time.Time{}
				case "invalid-window":
					s.usage.Buckets[0].Windows[0].UsedPercent = -1
				case "invalid-provider":
					s.usage.Provider = "unknown"
				}
			}
			s.runtimeMu.Unlock()
			if len(s.CachedQuotaSnapshot().RateLimits) != 0 {
				t.Fatal("unknown quota became usable")
			}
		})
	}
}
func TestQuotaMalformedNotificationInvalidatesUntilNewObservation(t *testing.T) {
	for _, raw := range []string{`{"rateLimits":{"primary":{"windowDurationMins":300}}}`, `{"rateLimits":{"primary":{"usedPercent":null}}}`, `{"rateLimits":{"primary":{"usedPercent":101}}}`, `{broken`} {
		s := &appServerSession{}
		s.alive.Store(true)
		quotaEvent(s, 10)
		old := s.cachedUsage().ObservedAt
		s.handleNotification("account/rateLimits/updated", json.RawMessage(raw))
		if len(s.CachedQuotaSnapshot().RateLimits) != 0 || !s.cachedUsage().ObservedAt.Equal(old) {
			t.Fatal("invalid event treated as new percentage")
		}
		quotaEvent(s, 10)
		if s.cachedUsage().Stale {
			t.Fatal("recovery failed")
		}
	}
}

type quotaReplyWriter struct {
	s       *appServerSession
	payload string
}

func (w quotaReplyWriter) Write(b []byte) (int, error) {
	var req struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return 0, err
	}
	if req.Method != "account/rateLimits/read" {
		return 0, fmt.Errorf("unexpected synthetic request")
	}
	w.s.handleResponse(rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(w.payload)})
	return len(b), nil
}
func (w quotaReplyWriter) Close() error { return nil }
func TestQuotaSuccessfulProtocolReadResetsStale(t *testing.T) {
	s := &appServerSession{}
	s.alive.Store(true)
	quotaEvent(s, 10)
	s.runtimeMu.Lock()
	s.usage.Stale = true
	s.runtimeMu.Unlock()
	s.stdin = quotaReplyWriter{s, `{"rateLimits":{"primary":{"usedPercent":96,"windowDurationMins":300}}}`}
	report, err := s.GetUsage(context.Background())
	if err != nil || report.Stale || report.ObservedAt.IsZero() || len(s.CachedQuotaSnapshot().RateLimits) != 1 {
		t.Fatal("successful read did not recover cache", err)
	}
}
func TestQuotaConcurrentCacheSnapshots(t *testing.T) {
	s := &appServerSession{}
	s.alive.Store(true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			quotaEvent(s, i%100)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = s.CachedQuotaSnapshot()
		}
	}()
	wg.Wait()
}
func TestQuotaExportForExistingGate(t *testing.T) {
	for _, used := range []int{10, 80, 81, 95, 96} {
		s := &appServerSession{}
		s.alive.Store(true)
		quotaEvent(s, used)
		b, err := json.Marshal(s.CachedQuotaSnapshot())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("QUOTA_CASE used_%d %s", used, b)
	}
	s := &appServerSession{}
	s.alive.Store(true)
	quotaEvent(s, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.GetUsage(ctx)
	b, _ := json.Marshal(s.CachedQuotaSnapshot())
	t.Logf("QUOTA_CASE refresh_failed %s", b)
}
