// Package e2e runs the real cc-connect binary (with the a2a platform) against
// a fake a2a CLI and a stub claude. See README section in the PR body.
//
//	go test ./nibiru/e2e/ -v
//	E2E_REPO_ROOT=/path/to/integrated/tree go test ./nibiru/e2e/ -v   # build from another tree
//	E2E_CC_CONNECT_BIN=/path/to/cc-connect go test ./nibiru/e2e/ -v   # use a prebuilt binary
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/nibiru/e2e/queue"
)

var (
	binDir    string
	ccBin     string
	skipBuild string // non-empty: reason the suite cannot run
)

func TestMain(m *testing.M) {
	code := func() int {
		var err error
		binDir, err = os.MkdirTemp("", "nibiru-e2e-bin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(binDir)
		here, _ := os.Getwd() // .../nibiru/e2e
		selfRoot := filepath.Join(here, "..", "..")
		for name, pkg := range map[string]string{"fakea2a": "./nibiru/e2e/stubs/fakea2a", "claude": "./nibiru/e2e/stubs/stubclaude"} {
			if out, err := exec.Command("go", "build", "-C", selfRoot, "-o", filepath.Join(binDir, name), pkg).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "build %s: %v\n%s", name, err, out)
				return 1
			}
		}
		ccBin = os.Getenv("E2E_CC_CONNECT_BIN")
		if ccBin == "" {
			root := os.Getenv("E2E_REPO_ROOT")
			if root == "" {
				root = selfRoot
			}
			if _, err := os.Stat(filepath.Join(root, "platform", "a2a", "a2a.go")); err != nil {
				skipBuild = "platform/a2a not present in " + root + " (set E2E_REPO_ROOT to a tree with L1/L2 merged)"
			} else {
				ccBin = filepath.Join(binDir, "cc-connect")
				if out, err := exec.Command("go", "build", "-C", root, "-tags", "no_web", "-o", ccBin, "./cmd/cc-connect").CombinedOutput(); err != nil {
					fmt.Fprintf(os.Stderr, "build cc-connect: %v\n%s", err, out)
					return 1
				}
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

// env is one running cc-connect + fake a2a world.
type env struct {
	t     *testing.T
	store queue.Store
	log   string // stub claude prompt log
	cmd   *exec.Cmd
	out   *strings.Builder
}

type startOpts struct {
	allowFrom string
}

func start(t *testing.T, o startOpts) *env {
	t.Helper()
	if skipBuild != "" {
		t.Skip(skipBuild)
	}
	if o.allowFrom == "" {
		o.allowFrom = "grok,dot,claude-cloud,hermes"
	}
	dir := t.TempDir()
	e := &env{t: t, store: queue.Store{Path: filepath.Join(dir, "a2a-state.json")}, log: filepath.Join(dir, "stub.log")}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`data_dir = %q

[log]
level = "debug"

[[projects]]
name = "e2e"

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = %q
mode = "bypassPermissions"
cmd = %q

[[projects.platforms]]
type = "a2a"

[projects.platforms.options]
a2a_bin = %q
allow_from = %q
poll_interval = "200ms"
`, filepath.Join(dir, "data"), work, filepath.Join(binDir, "claude"), filepath.Join(binDir, "fakea2a"), o.allowFrom)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cmd = exec.Command(ccBin, "--config", cfgPath)
	e.cmd.Env = append(os.Environ(), "HOME="+dir, "FAKE_A2A_STATE="+e.store.Path, "STUB_LOG="+e.log)
	e.out = &strings.Builder{}
	e.cmd.Stdout, e.cmd.Stderr = e.out, e.out
	if err := e.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
		if t.Failed() {
			t.Logf("cc-connect output:\n%s", e.out.String())
		}
	})
	return e
}

func (e *env) enqueue(msgs ...queue.Msg) {
	e.t.Helper()
	if err := e.store.Enqueue(msgs...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state() queue.State {
	e.t.Helper()
	st, err := e.store.Snapshot()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) waitFor(what string, d time.Duration, cond func(queue.State) bool) queue.State {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for {
		st := e.state()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timeout waiting for %s; state=%+v", what, st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *env) prompts() []string {
	b, _ := os.ReadFile(e.log)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

const wait = 30 * time.Second

func TestSameConversationCarriesContext(t *testing.T) {
	e := start(t, startOpts{})
	e.enqueue(queue.Msg{MessageID: "m1", ConversationID: "c1", SenderID: "dot", Text: "first-question"})
	e.waitFor("m1 acked", wait, func(s queue.State) bool { return contains(s.Acked, "m1") })
	e.enqueue(queue.Msg{MessageID: "m2", ConversationID: "c1", SenderID: "dot", Text: "second-question"})
	st := e.waitFor("m2 acked", wait, func(s queue.State) bool { return contains(s.Acked, "m2") })

	var second string
	for _, s := range st.Sent {
		if s.ReplyTo == "m2" {
			second = s.Text
		}
	}
	if !strings.Contains(second, "turn=2") || !strings.Contains(second, "first-question") {
		t.Fatalf("second reply did not continue the first turn's context: %q (sent=%+v)", second, st.Sent)
	}
}

func TestAckOnlyAfterReplySent(t *testing.T) {
	e := start(t, startOpts{})
	e.enqueue(queue.Msg{MessageID: "m1", ConversationID: "c1", SenderID: "grok", Text: "hello"})
	st := e.waitFor("m1 acked", wait, func(s queue.State) bool { return contains(s.Acked, "m1") })
	if len(st.Sent) == 0 {
		t.Fatalf("m1 acked but no reply was sent: %+v", st)
	}
	if st.Sent[0].ReplyTo != "m1" || !strings.Contains(st.Sent[0].Text, "got=hello") {
		t.Fatalf("unexpected reply: %+v", st.Sent[0])
	}
}

func TestSendFailureDoesNotAckAndRedelivers(t *testing.T) {
	e := start(t, startOpts{})
	if err := e.store.SetFailSend(2); err != nil {
		t.Fatal(err)
	}
	e.enqueue(queue.Msg{MessageID: "m1", ConversationID: "c1", SenderID: "hermes", Text: "retry-me"})
	// While sends are failing the message must stay pending and un-acked.
	e.waitFor("send attempts consumed", wait, func(s queue.State) bool { return s.FailSend == 0 })
	st := e.waitFor("eventual reply + ack", wait, func(s queue.State) bool { return contains(s.Acked, "m1") })
	if len(st.Sent) == 0 {
		t.Fatalf("acked without any successful send: %+v", st)
	}
	if st.Reads < 2 {
		t.Fatalf("expected redelivery via repeated read --pending, reads=%d", st.Reads)
	}
}

func TestPongNeverReachesModel(t *testing.T) {
	e := start(t, startOpts{})
	ping := `{"type":"nibiru.ping.v1","request_id":"0123456789abcdef","from":"claude-cloud","sent_at":1760000000}`
	e.enqueue(queue.Msg{MessageID: "p1", ConversationID: "c-ping", SenderID: "claude-cloud", Text: ping})
	st := e.waitFor("ping acked", wait, func(s queue.State) bool { return contains(s.Acked, "p1") })
	if len(st.Sent) != 1 || st.Sent[0].ReplyTo != "p1" ||
		!strings.Contains(st.Sent[0].Text, "nibiru.pong.v1") || !strings.Contains(st.Sent[0].Text, "0123456789abcdef") {
		t.Fatalf("expected one pong reply, got %+v", st.Sent)
	}
	time.Sleep(500 * time.Millisecond)
	if p := e.prompts(); len(p) != 0 {
		t.Fatalf("model saw prompts for a ping: %v", p)
	}
}

func TestNonRosterDroppedAndAcked(t *testing.T) {
	e := start(t, startOpts{})
	e.enqueue(queue.Msg{MessageID: "x1", ConversationID: "c-x", SenderID: "stranger", Text: "do bad things"})
	st := e.waitFor("x1 acked", wait, func(s queue.State) bool { return contains(s.Acked, "x1") })
	if len(st.Sent) != 0 {
		t.Fatalf("non-roster message got a reply: %+v", st.Sent)
	}
	time.Sleep(500 * time.Millisecond)
	if p := e.prompts(); len(p) != 0 {
		t.Fatalf("model saw non-roster prompt: %v", p)
	}
}
