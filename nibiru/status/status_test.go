package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var now = time.Date(2026, 10, 15, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

const logSample = `{"ts": "2026-10-15T19:28:04+08:00", "event": "claude_run", "kind": "batch", "conversation_id": "ac-aaa", "message_ids": ["m1"], "text": "SECRET BODY"}
Traceback (most recent call last): not json
{"ts": "2026-10-15T19:29:09+08:00", "event": "alert", "conversation_id": "ac-bbb", "reason": "gave up"}
{"ts": "2026-10-15T19:30:27+08:00", "event": "claude_done", "conversation_id": "ac-aaa", "ok": true}
`

const workersSample = "id\tissue\tpid\tworktree\tmodel\tstart_epoch\tstart\n" +
	"w1\t756\t111\t/x/w1\tsonnet\t1792063792\t2026-10-15T19:29:53+0800\n" +
	"w2\t843\t222\t/x/w2\topus\t1792063809\t2026-10-15T19:30:09+0800\n"

func src() Sources {
	return Sources{
		Now:        func() time.Time { return now },
		RunnerLog:  func() (string, error) { return logSample, nil },
		WorkersTSV: func() (string, error) { return workersSample, nil },
		PidAlive:   func(pid int) bool { return pid == 111 },
		MergedPRs: func(ctx context.Context, since time.Time) ([]PR, error) {
			return []PR{{Number: 900, Title: "fix a", MergedAt: now.Add(-time.Hour)}}, nil
		},
	}
}

func TestGenerateSections(t *testing.T) {
	out := Generate(context.Background(), src(), Options{})
	for _, want := range []string{"runner", "alert", "ac-bbb", "w1", "756", "存活", "w2", "已退出", "#900", "fix a"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNoMessageBody(t *testing.T) {
	out := Generate(context.Background(), src(), Options{})
	if strings.Contains(out, "SECRET BODY") || strings.Contains(out, "not json") {
		t.Fatalf("leaked body/non-json:\n%s", out)
	}
}

func TestSourceFailuresSkipped(t *testing.T) {
	s := src()
	s.RunnerLog = func() (string, error) { return "", errors.New("nope") }
	s.WorkersTSV = func() (string, error) { return "", errors.New("nope") }
	s.MergedPRs = func(context.Context, time.Time) ([]PR, error) { return nil, errors.New("gh down") }
	out := Generate(context.Background(), s, Options{})
	if out == "" || !strings.Contains(out, "不可用") {
		t.Fatalf("expected graceful notes, got %q", out)
	}
}

func TestLimitRunes(t *testing.T) {
	s := src()
	s.MergedPRs = func(context.Context, time.Time) ([]PR, error) {
		var prs []PR
		for i := 0; i < 400; i++ {
			prs = append(prs, PR{Number: i, Title: strings.Repeat("长", 40), MergedAt: now})
		}
		return prs, nil
	}
	out := Generate(context.Background(), s, Options{})
	if n := utf8.RuneCountInString(out); n > 1500 {
		t.Fatalf("len %d > 1500", n)
	}
	if !strings.Contains(out, "截断") {
		t.Fatal("no truncation marker")
	}
}

func TestRecentEventsLimitAndOrder(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(`{"ts": "2026-10-15T19:00:00+08:00", "event": "e` + string(rune('a'+i%26)) + `", "conversation_id": "c"}` + "\n")
	}
	s := src()
	s.RunnerLog = func() (string, error) { return b.String(), nil }
	out := Generate(context.Background(), s, Options{MaxEvents: 5})
	if strings.Count(out, "conversation=c") != 5 {
		t.Fatalf("want 5 events:\n%s", out)
	}
}

func TestParseWorkersSkipsBadRows(t *testing.T) {
	ws := ParseWorkers("id\tissue\tpid\n\nbad\nw\t1\tx\tw\tm\n")
	if len(ws) != 0 {
		t.Fatalf("unexpected %v", ws)
	}
}
