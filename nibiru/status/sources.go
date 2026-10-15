package status

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Runner executes an external command and returns stdout (injectable for tests).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

const tailBytes = 256 << 10

// DefaultDir is ~/.config/nibiru-a2a.
func DefaultDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "nibiru-a2a")
}

// readTail returns the last tailBytes of a file (runner.log can be large).
func readTail(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if st.Size() > tailBytes {
		off = st.Size() - tailBytes
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return "", err
	}
	return string(buf), nil
}

// Live builds production sources rooted at dir; run executes gh.
func Live(dir, repo string, run Runner) Sources {
	if run == nil {
		run = ExecRunner
	}
	return Sources{
		Now:       time.Now,
		RunnerLog: func() (string, error) { return readTail(filepath.Join(dir, "runner.log")) },
		WorkersTSV: func() (string, error) {
			b, err := os.ReadFile(filepath.Join(dir, "workers.tsv"))
			return string(b), err
		},
		PidAlive: func(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil },
		MergedPRs: func(ctx context.Context, since time.Time) ([]PR, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			out, err := run(ctx, "gh", "pr", "list", "--repo", repo, "--state", "merged",
				"--limit", "30", "--json", "number,title,mergedAt")
			if err != nil {
				return nil, fmt.Errorf("status: gh pr list: %w", err)
			}
			return ParsePRs(out, since)
		},
	}
}

// ParsePRs parses `gh pr list --json number,title,mergedAt`, keeping PRs merged after since.
func ParsePRs(raw []byte, since time.Time) ([]PR, error) {
	var rows []struct {
		Number   int       `json:"number"`
		Title    string    `json:"title"`
		MergedAt time.Time `json:"mergedAt"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("status: parse prs: %w", err)
	}
	var out []PR
	for _, r := range rows {
		if r.MergedAt.After(since) {
			out = append(out, PR{r.Number, r.Title, r.MergedAt})
		}
	}
	return out, nil
}
