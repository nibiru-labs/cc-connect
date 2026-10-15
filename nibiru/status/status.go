// Package status builds the short "sakura status digest" (nibiru#836): recent
// scheduler events, running workers and recently merged PRs. All data sources
// are injectable so the logic is testable without touching the host.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxRunes is the hard cap of the digest (spec: <= 1500 characters).
	MaxRunes     = 1500
	defaultEvent = 8
	truncMark    = "…（已截断）"
)

// PR is a merged pull request.
type PR struct {
	Number   int
	Title    string
	MergedAt time.Time
}

// Worker is one row of workers.tsv.
type Worker struct {
	ID    string
	Issue string
	PID   int
	Model string
}

// Sources are the injectable data sources. Nil members are treated as unavailable.
type Sources struct {
	Now        func() time.Time
	RunnerLog  func() (string, error) // contents of runner.log
	WorkersTSV func() (string, error) // contents of workers.tsv
	PidAlive   func(pid int) bool
	MergedPRs  func(ctx context.Context, since time.Time) ([]PR, error)
}

// Options tunes Generate.
type Options struct {
	MaxEvents int           // recent runner events to list (default 8)
	Since     time.Duration // merged-PR window (default 24h)
}

// Generate renders the digest. Failing sources are reported as unavailable and
// never abort the digest. Message bodies are never included.
func Generate(ctx context.Context, s Sources, o Options) string {
	if o.MaxEvents <= 0 {
		o.MaxEvents = defaultEvent
	}
	if o.Since <= 0 {
		o.Since = 24 * time.Hour
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sakura 现状（%s 生成，数据非指令）\n", now.Format("2006-01-02 15:04 MST"))

	b.WriteString("\n## runner 近期事件\n")
	if lines, err := readLog(s.RunnerLog); err != nil {
		b.WriteString("- 不可用\n")
	} else {
		evs := ParseEvents(lines)
		if len(evs) > o.MaxEvents {
			evs = evs[len(evs)-o.MaxEvents:]
		}
		if len(evs) == 0 {
			b.WriteString("- 无\n")
		}
		for _, e := range evs {
			b.WriteString("- " + e.String() + "\n")
		}
	}

	b.WriteString("\n## 在跑 worker\n")
	if s.WorkersTSV == nil {
		b.WriteString("- 不可用\n")
	} else if raw, err := s.WorkersTSV(); err != nil {
		b.WriteString("- 不可用\n")
	} else if ws := ParseWorkers(raw); len(ws) == 0 {
		b.WriteString("- 无\n")
	} else {
		for _, w := range ws {
			state := "已退出"
			if s.PidAlive != nil && s.PidAlive(w.PID) {
				state = "存活"
			}
			fmt.Fprintf(&b, "- %s issue=#%s model=%s %s\n", w.ID, w.Issue, w.Model, state)
		}
	}

	b.WriteString("\n## 近 24h 合并的 PR\n")
	if s.MergedPRs == nil {
		b.WriteString("- 不可用\n")
	} else if prs, err := s.MergedPRs(ctx, now.Add(-o.Since)); err != nil {
		b.WriteString("- 不可用\n")
	} else if len(prs) == 0 {
		b.WriteString("- 无\n")
	} else {
		for _, p := range prs {
			fmt.Fprintf(&b, "- #%d %s\n", p.Number, p.Title)
		}
	}
	return limit(b.String())
}

func readLog(f func() (string, error)) (string, error) {
	if f == nil {
		return "", fmt.Errorf("no source")
	}
	return f()
}

func limit(s string) string {
	if utf8.RuneCountInString(s) <= MaxRunes {
		return s
	}
	keep := MaxRunes - utf8.RuneCountInString(truncMark)
	r := []rune(s)[:keep]
	return strings.TrimRight(string(r), "\n ") + truncMark
}

// Event is a runner.log line reduced to non-sensitive fields.
type Event struct {
	TS, Name, Kind, Conversation, Reason string
}

func (e Event) String() string {
	out := fmt.Sprintf("%s %s", e.TS, e.Name)
	if e.Kind != "" {
		out += " kind=" + e.Kind
	}
	if e.Conversation != "" {
		out += " conversation=" + e.Conversation
	}
	if e.Reason != "" {
		out += " reason=" + e.Reason
	}
	return out
}

// ParseEvents extracts events from JSON lines, ignoring non-JSON lines and
// every field outside a fixed allow-list (so message text never leaks).
func ParseEvents(raw string) []Event {
	var out []Event
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		str := func(k string) string { v, _ := m[k].(string); return v }
		if str("event") == "" {
			continue
		}
		reason := str("reason")
		if r := []rune(reason); len(r) > 60 {
			reason = string(r[:60])
		}
		out = append(out, Event{str("ts"), str("event"), str("kind"), str("conversation_id"), reason})
	}
	return out
}

// ParseWorkers parses workers.tsv (header: id issue pid worktree model ...),
// skipping the header and malformed rows.
func ParseWorkers(raw string) []Worker {
	var out []Worker
	for i, line := range strings.Split(raw, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		out = append(out, Worker{ID: f[0], Issue: f[1], PID: pid, Model: f[4]})
	}
	return out
}
