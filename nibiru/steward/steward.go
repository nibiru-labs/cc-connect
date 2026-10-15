// Package steward implements the nibiru#836 M3 lifecycle steward: when a
// cc-connect session's context usage crosses a threshold (default 85%), it
// writes a handoff summary to <data_dir>/nibiru-handoff/<session_key>.md,
// starts a fresh session and injects the summary as its first context.
//
// It talks to cc-connect only through the management HTTP API and never
// touches the engine.
package steward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultThreshold       = 0.85
	DefaultMaxSummaryRunes = 2000
	DefaultCooldown        = 30 * time.Minute
	handoffDirName         = "nibiru-handoff"
	truncMark              = "…[已截断]"
)

// SessionInfo is one cc-connect session as seen by the steward.
type SessionInfo struct {
	Project       string
	ID            string
	SessionKey    string
	Name          string
	UsedTokens    int
	ContextWindow int
	// Percent is an already-computed usage percentage (0-100), if the API reports one.
	Percent float64
}

// HistoryItem is one message of a session transcript.
type HistoryItem struct {
	Role    string
	Content string
}

// API is what the steward needs from cc-connect.
type API interface {
	ListSessions(ctx context.Context, project string) ([]SessionInfo, error)
	History(ctx context.Context, project, sessionID string, limit int) ([]HistoryItem, error)
	// NewSession starts a fresh session for sessionKey and injects contextText
	// as its first context.
	NewSession(ctx context.Context, project, sessionKey, name, contextText string) error
}

// Config controls the steward.
type Config struct {
	Projects        []string
	DataDir         string
	Threshold       float64       // 0<t<=1, default 0.85
	MaxSummaryRunes int           // default 2000
	Cooldown        time.Duration // min gap between rotations of one session
	HistoryLimit    int           // messages fetched for the summary, default 40
}

// Result describes the outcome for one session that crossed the threshold.
type Result struct {
	Project, SessionKey string
	Rotated             bool
	HandoffFile         string
	Err                 error
}

// Steward polls usage and rotates sessions.
type Steward struct {
	api API
	cfg Config

	mu      sync.Mutex
	lastRot map[string]time.Time
	now     func() time.Time
}

// New builds a Steward, filling in defaults.
func New(api API, cfg Config) *Steward {
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		cfg.Threshold = DefaultThreshold
	}
	if cfg.MaxSummaryRunes <= 0 {
		cfg.MaxSummaryRunes = DefaultMaxSummaryRunes
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 40
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	return &Steward{api: api, cfg: cfg, lastRot: map[string]time.Time{}, now: time.Now}
}

// ShouldRotate reports whether the session is at or above threshold. ok is
// false when the session reports no usable usage data.
func ShouldRotate(s SessionInfo, threshold float64) (rotate, ok bool) {
	var ratio float64
	switch {
	case s.Percent > 0:
		ratio = s.Percent / 100
	case s.ContextWindow > 0:
		ratio = float64(s.UsedTokens) / float64(s.ContextWindow)
	default:
		return false, false
	}
	return ratio >= threshold, true
}

// TruncateSummary cuts s to at most max runes (marker included) on a rune boundary.
func TruncateSummary(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	keep := max - utf8.RuneCountInString(truncMark)
	if keep < 0 {
		keep = 0
	}
	r := []rune(s)
	return string(r[:keep]) + truncMark
}

// BuildSummary produces an extractive handoff summary from the transcript,
// preferring the most recent messages, within max runes.
func BuildSummary(h []HistoryItem, max int) string {
	const header = "# 交接摘要（上一会话最近进展）\n\n"
	budget := max - utf8.RuneCountInString(header)
	if budget < 0 {
		budget = 0
	}
	var lines []string
	used := 0
	for i := len(h) - 1; i >= 0; i-- {
		line := fmt.Sprintf("- [%s] %s\n", h[i].Role, strings.TrimSpace(h[i].Content))
		n := utf8.RuneCountInString(line)
		if used+n > budget {
			if len(lines) == 0 { // newest message alone is too big: keep its tail
				r := []rune(line)
				lines = append(lines, string(r[len(r)-budget:]))
			}
			break
		}
		lines = append(lines, line)
		used += n
	}
	var b strings.Builder
	b.WriteString(header)
	for i := len(lines) - 1; i >= 0; i-- {
		b.WriteString(lines[i])
	}
	return TruncateSummary(b.String(), max)
}

// HandoffPath returns <dataDir>/nibiru-handoff/<session_key>.md with the key made filesystem-safe.
func HandoffPath(dataDir, sessionKey string) string {
	name := url.PathEscape(sessionKey)
	name = strings.ReplaceAll(name, "..", "_")
	return filepath.Join(dataDir, handoffDirName, name+".md")
}

// InjectionText wraps the summary as the first message of the new session.
func InjectionText(summary string) string {
	return "以下是上一会话的交接摘要，请据此继续工作：\n\n" + summary
}

// RunOnce performs a single poll over all configured projects.
func (s *Steward) RunOnce(ctx context.Context) ([]Result, error) {
	var out []Result
	var errs []error
	for _, p := range s.cfg.Projects {
		sessions, err := s.api.ListSessions(ctx, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("steward: list %s: %w", p, err))
			continue
		}
		for _, sess := range sessions {
			if sess.SessionKey == "" {
				continue
			}
			rot, ok := ShouldRotate(sess, s.cfg.Threshold)
			if !ok || !rot || s.coolingDown(p, sess.SessionKey) {
				continue
			}
			r := s.rotate(ctx, p, sess)
			if r.Err != nil {
				errs = append(errs, r.Err)
			}
			out = append(out, r)
		}
	}
	return out, errors.Join(errs...)
}

func (s *Steward) coolingDown(project, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastRot[project+"\x00"+key]
	return ok && s.now().Sub(t) < s.cfg.Cooldown
}

func (s *Steward) rotate(ctx context.Context, project string, sess SessionInfo) Result {
	r := Result{Project: project, SessionKey: sess.SessionKey}
	hist, err := s.api.History(ctx, project, sess.ID, s.cfg.HistoryLimit)
	if err != nil {
		r.Err = fmt.Errorf("steward: history %s/%s: %w", project, sess.SessionKey, err)
		return r
	}
	summary := BuildSummary(hist, s.cfg.MaxSummaryRunes)
	path := HandoffPath(s.cfg.DataDir, sess.SessionKey)
	if err := writeAtomic(path, summary); err != nil {
		r.Err = fmt.Errorf("steward: write handoff: %w", err)
		return r
	}
	r.HandoffFile = path
	if err := s.api.NewSession(ctx, project, sess.SessionKey, sess.Name, InjectionText(summary)); err != nil {
		r.Err = fmt.Errorf("steward: new session %s/%s: %w", project, sess.SessionKey, err)
		return r
	}
	s.mu.Lock()
	s.lastRot[project+"\x00"+sess.SessionKey] = s.now()
	s.mu.Unlock()
	r.Rotated = true
	slog.Info("steward: session rotated", "project", project, "session_key", sess.SessionKey, "handoff", path)
	return r
}

func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".handoff-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
