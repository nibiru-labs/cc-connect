package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrintCronAddUsage_DocumentsSilent is a help-text guard for the
// `cc-connect cron add --silent` flag. The flag is parsed in runCronAdd
// (cmd/cc-connect/cron.go) and sets body["silent"] = true on the /cron/add
// request; if printCronAddUsage ever drifts and stops mentioning it, users
// who run `cc-connect cron add --help` will not discover the feature even
// though it still works.
//
// This complements TestParseCronEditValue (cron_edit_test.go), which guards
// the symmetric `cron edit <id> silent` path. The behavioural round-trip
// (CLI flag → request body → server → CronJob.Silent) is covered by the
// CronAddRequest / CronJob tests in core/api_test.go and core/cron_test.go.
func TestPrintCronAddUsage_DocumentsSilent(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	printCronAddUsage()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	help := string(out)

	if !strings.Contains(help, "--silent") {
		t.Errorf("printCronAddUsage does not document --silent; got:\n%s", help)
	}
	if !strings.Contains(help, "Suppress cron start notification") {
		t.Errorf("printCronAddUsage does not describe --silent; got:\n%s", help)
	}
	if !strings.Contains(help, "cc-connect cron add --cron \"0 9 * * *\" --prompt \"Daily standup reminder\" --silent") {
		t.Errorf("printCronAddUsage does not include --silent example; got:\n%s", help)
	}
}

func TestCronAddDoesNotChooseUnifiedRouteFromStaleAgentEnvironment(t *testing.T) {
	if os.Getenv("CC_CONNECT_CRON_ADD_TEST_HELPER") == "1" {
		runCronAdd([]string{
			"--data-dir", os.Getenv("CC_CONNECT_CRON_ADD_TEST_DATA_DIR"),
			"--cron", "0 6 * * *", "--prompt", "prepare report",
		})
		return
	}

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(dataDir, "run", "api.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan []byte, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cron/add" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cron-1","cron_expr":"0 6 * * *","prompt":"prepare report"}`)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	t.Setenv("CC_PROJECT", "sakura")
	t.Setenv("CC_SESSION_KEY", "unified:sakura")
	t.Setenv("CC_ROUTE_SESSION_KEY", "a2a:c1")
	t.Setenv("CC_CONNECT_CRON_ADD_TEST_HELPER", "1")
	t.Setenv("CC_CONNECT_CRON_ADD_TEST_DATA_DIR", dataDir)

	cmd := exec.Command(os.Args[0], "-test.run=^TestCronAddDoesNotChooseUnifiedRouteFromStaleAgentEnvironment$")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cron add helper failed: %v\n%s", err, output)
	}

	select {
	case body := <-requests:
		var req struct {
			Project         string `json:"project"`
			SessionKey      string `json:"session_key"`
			RouteSessionKey string `json:"route_session_key"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode /cron/add body: %v", err)
		}
		if req.Project != "sakura" || req.SessionKey != "unified:sakura" {
			t.Fatalf("API identity = project %q session %q, want sakura/unified:sakura", req.Project, req.SessionKey)
		}
		if req.RouteSessionKey != "" {
			t.Fatalf("API route_session_key = %q, want no stale process-start route", req.RouteSessionKey)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for CLI /cron/add request")
	}
}
