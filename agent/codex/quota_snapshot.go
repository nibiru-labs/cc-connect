package codex

import (
	"encoding/json"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"time"
)

// Missing/null percentages must never become a default zero-used quota.
func (w *appServerRateLimitWindow) UnmarshalJSON(data []byte) error {
	var raw struct {
		UsedPercent        *int  `json:"usedPercent"`
		WindowDurationMins int   `json:"windowDurationMins"`
		ResetsAt           int64 `json:"resetsAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.UsedPercent == nil || *raw.UsedPercent < 0 || *raw.UsedPercent > 100 {
		return fmt.Errorf("invalid quota percentage")
	}
	*w = appServerRateLimitWindow{UsedPercent: *raw.UsedPercent, WindowDurationMins: raw.WindowDurationMins, ResetsAt: raw.ResetsAt}
	return nil
}

func (s *appServerSession) CachedQuotaSnapshot() core.QuotaSnapshot {
	result := core.UnknownQuotaSnapshot()
	report := s.cachedUsage()
	if report == nil {
		return result
	}
	result.AsOf = report.ObservedAt // Reads never restamp the source observation.
	age := time.Since(report.ObservedAt)
	if !s.Alive() || report.Provider != "codex" || report.Stale || report.ObservedAt.IsZero() || age > 120*time.Second || age < -2*time.Second {
		return result
	}
	for _, bucket := range report.Buckets {
		for _, window := range bucket.Windows {
			if window.UsedPercent < 0 || window.UsedPercent > 100 || window.WindowSeconds <= 0 {
				result.RateLimits = map[string]core.QuotaWindow{}
				return result
			}
			used := window.UsedPercent
			if !bucket.Allowed || bucket.LimitReached {
				used = 100
			}
			result.RateLimits[fmt.Sprintf("window_%d", len(result.RateLimits))] = core.QuotaWindow{UsedPercent: used}
		}
	}
	return result
}
