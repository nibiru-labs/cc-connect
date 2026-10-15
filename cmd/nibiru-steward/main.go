// Command nibiru-steward rotates cc-connect sessions nearing their context
// limit (nibiru#836 M3). See package nibiru/steward.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/nibiru/steward"
)

func main() {
	var (
		base      = flag.String("api", "http://127.0.0.1:9820", "cc-connect management API base URL")
		tokenEnv  = flag.String("token-env", "CC_MANAGEMENT_TOKEN", "env var holding the management token")
		projects  = flag.String("projects", "", "comma-separated project names (required)")
		dataDir   = flag.String("data-dir", defaultDataDir(), "cc-connect data_dir (handoffs go to <data-dir>/nibiru-handoff)")
		threshold = flag.Float64("threshold", steward.DefaultThreshold, "rotation threshold, 0-1")
		maxRunes  = flag.Int("max-summary", steward.DefaultMaxSummaryRunes, "max summary length in runes")
		interval  = flag.Duration("interval", time.Minute, "poll interval")
		cooldown  = flag.Duration("cooldown", steward.DefaultCooldown, "min gap between rotations of one session")
		once      = flag.Bool("once", false, "run a single poll and exit")
	)
	flag.Parse()
	if *projects == "" {
		slog.Error("nibiru-steward: -projects is required")
		os.Exit(2)
	}
	cfg := steward.Config{
		Projects: splitCSV(*projects), DataDir: *dataDir, Threshold: *threshold,
		MaxSummaryRunes: *maxRunes, Cooldown: *cooldown,
	}
	st := steward.New(steward.NewHTTPClient(*base, os.Getenv(*tokenEnv), nil), cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if _, err := st.RunOnce(ctx); err != nil {
			slog.Warn("nibiru-steward: poll", "error", err)
		}
		if *once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

func defaultDataDir() string {
	h, _ := os.UserHomeDir()
	return h + "/.cc-connect"
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
