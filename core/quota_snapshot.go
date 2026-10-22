package core

import (
	"net/http"
	"time"
)

// CachedQuotaSnapshotProvider must only read an already-observed cache.
// It must not refresh provider state or inspect account/credential files.
type CachedQuotaSnapshotProvider interface{ CachedQuotaSnapshot() QuotaSnapshot }
type QuotaWindow struct {
	UsedPercent int `json:"used_percent"`
}
type QuotaSnapshot struct {
	Source     string                 `json:"source"`
	AsOf       time.Time              `json:"as_of"`
	RateLimits map[string]QuotaWindow `json:"rate_limits"`
}

func UnknownQuotaSnapshot() QuotaSnapshot {
	return QuotaSnapshot{Source: "codex app-server received cache", RateLimits: map[string]QuotaWindow{}}
}
func (s *APIServer) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result := UnknownQuotaSnapshot()
	// No account identity is exported. Multiple sources cannot be disambiguated,
	// so do not arbitrarily pick a session or claim account-global quota.
	sessions := s.runtimeSessions()
	if len(sessions) == 1 && sessions[0] != nil {
		if provider, ok := sessions[0].(CachedQuotaSnapshotProvider); ok {
			result = provider.CachedQuotaSnapshot()
		}
	}
	apiJSON(w, http.StatusOK, result)
}
