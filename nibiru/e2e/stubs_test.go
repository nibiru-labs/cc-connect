package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/nibiru/e2e/queue"
)

// The stubs themselves are covered here so a scenario failure can be blamed
// on cc-connect rather than on the harness.

func TestFakeA2ARoundTrip(t *testing.T) {
	st := queue.Store{Path: filepath.Join(t.TempDir(), "s.json")}
	run := func(args ...string) (string, error) {
		c := exec.Command(filepath.Join(binDir, "fakea2a"), args...)
		c.Env = append(os.Environ(), "FAKE_A2A_STATE="+st.Path)
		out, err := c.Output()
		return string(out), err
	}
	if err := st.Enqueue(queue.Msg{MessageID: "m1", ConversationID: "c", SenderID: "dot", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	out, err := run("read", "--pending")
	if err != nil || !strings.Contains(out, `"m1"`) {
		t.Fatalf("read: %v %q", err, out)
	}
	_ = st.SetFailSend(1)
	if _, err := run("send", "--reply-to", "m1", "--key", "m1-1", "reply"); err == nil {
		t.Fatal("expected simulated send failure")
	}
	if _, err := run("send", "--reply-to", "m1", "--key", "m1-1", "reply"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("send", "--reply-to", "m1", "--key", "m1-1", "reply"); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := run("ack", "m1"); err != nil {
		t.Fatal(err)
	}
	s, _ := st.Snapshot()
	if len(s.Pending) != 0 || len(s.Acked) != 1 || len(s.Sent) != 1 || s.Reads != 1 {
		t.Fatalf("unexpected state %+v", s)
	}
}

func TestStubClaudeCarriesContext(t *testing.T) {
	c := exec.Command(filepath.Join(binDir, "claude"))
	c.Env = append(os.Environ(), "STUB_LOG="+filepath.Join(t.TempDir(), "l"))
	c.Stdin = strings.NewReader(
		`{"type":"user","message":{"role":"user","content":"one"}}` + "\n" +
			`{"type":"user","message":{"role":"user","content":"two"}}` + "\n")
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "turn=2 prev=one got=two") {
		t.Fatalf("stub output: %s", out)
	}
}
