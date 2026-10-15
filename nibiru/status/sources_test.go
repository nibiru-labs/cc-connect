package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePRsFiltersBySince(t *testing.T) {
	raw := []byte(`[{"number":1,"title":"new","mergedAt":"2026-10-15T10:00:00Z"},{"number":2,"title":"old","mergedAt":"2026-10-01T10:00:00Z"}]`)
	prs, err := ParsePRs(raw, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	if err != nil || len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("got %v %v", prs, err)
	}
	if _, err := ParsePRs([]byte("x"), time.Now()); err == nil {
		t.Fatal("want error")
	}
}

func TestLiveReadsFilesAndUsesRunner(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", tailBytes+10) + "\n{\"event\":\"tail\"}\n"
	os.WriteFile(filepath.Join(dir, "runner.log"), []byte(big), 0o600)
	os.WriteFile(filepath.Join(dir, "workers.tsv"), []byte("h\n"), 0o600)
	var gotArgs []string
	s := Live(dir, "o/r", func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte(`[]`), nil
	})
	log, err := s.RunnerLog()
	if err != nil || !strings.Contains(log, `"tail"`) || len(log) > tailBytes {
		t.Fatalf("tail: %v len=%d", err, len(log))
	}
	if _, err := s.MergedPRs(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if gotArgs[0] != "gh" || !strings.Contains(strings.Join(gotArgs, " "), "--repo o/r") {
		t.Fatalf("args %v", gotArgs)
	}
	if !s.PidAlive(os.Getpid()) || s.PidAlive(0) {
		t.Fatal("pidalive")
	}
	s2 := Live(t.TempDir(), "o/r", func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("x") })
	if _, err := s2.RunnerLog(); err == nil {
		t.Fatal("want missing-file error")
	}
	if _, err := s2.MergedPRs(context.Background(), time.Now()); err == nil {
		t.Fatal("want gh error")
	}
}
