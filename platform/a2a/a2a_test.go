package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func in(id, conv, sender, text string) Inbound {
	return Inbound{MessageID: id, ConversationID: conv, SenderID: sender, Text: text}
}

func startP(t *testing.T, c Client, a Acker, extra map[string]any) (*Platform, *recorder) {
	t.Helper()
	p := newTestPlatform(t, c, a, extra)
	p.pong = NewPongHandler(c, 0).HandleSettled // New() wires this; newPlatform leaves it nil
	r := &recorder{}
	if err := p.Start(r.handle); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, r
}

// 规格: SessionKey="a2a:"+conversation_id; UserID=sender_id; Platform="a2a"
func TestMessageMapping(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "hello")}}
	p, r := startP(t, c, newFakeAcker(), nil)
	eventually(t, "message delivered", func() bool { return len(r.all()) == 1 })
	m := r.all()[0]
	if m.SessionKey != "a2a:c1" || m.UserID != "grok" || m.Platform != "a2a" || m.MessageID != "m1" || m.Content != "hello" {
		t.Fatalf("bad mapping: %+v", m)
	}
	rc, ok := m.ReplyCtx.(replyContext)
	if !ok && m.ReplyCtx != nil {
		if prc, pok := m.ReplyCtx.(*replyContext); pok {
			rc, ok = *prc, true
		}
	}
	if !ok || rc.ConversationID != "c1" || len(rc.MessageIDs) != 1 || rc.MessageIDs[0] != "m1" {
		t.Fatalf("bad replyCtx: %#v", m.ReplyCtx)
	}
	if p.Name() != "a2a" {
		t.Fatalf("Name=%q", p.Name())
	}
}

// 规格: 只接名册成员; 非名册 记日志、ack、丢弃, 不进模型
func TestRosterFilterDropsAndAcks(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "stranger", "hi"), in("m2", "c2", "dot", "yo")}}
	_, r := startP(t, c, newFakeAcker(), nil)
	eventually(t, "roster msg delivered", func() bool { return len(r.all()) == 1 })
	eventually(t, "stranger acked", func() bool {
		for _, id := range c.ackedCopy() {
			if id == "m1" {
				return true
			}
		}
		return false
	})
	if got := r.all()[0].UserID; got != "dot" {
		t.Fatalf("delivered %q", got)
	}
}

// Regression: the pre-receiver path forwarded each bounded roster piece to the
// model as an independent A2A message. An incomplete group must stay in the
// platform layer and produce no model input.
func TestRosterTransferIncompletePartsDoNotReachModel(t *testing.T) {
	digest := strings.Repeat("a", 64)
	manifest := fmt.Sprintf(`{"type":"xstation.roster.manifest.v1","version":2,"issued_by":"saka","approved_by":"user","payload_format":"source-roster-json-utf8","source_sha256":%q,"payload_sha256":%q,"payload_bytes":32,"chunks":2}`, digest, digest)
	chunk := fmt.Sprintf(`{"type":"xstation.roster.chunk.v1","version":2,"issued_by":"saka","source_sha256":%q,"payload_sha256":%q,"index":0,"total":2,"text":"{"}`, digest, digest)
	p, _ := newRosterReceiverPlatform(t, filepath.Join(t.TempDir(), "roster-state.json"), time.Now)
	r := &recorder{}
	p.dispatch(context.Background(), r.handle, in("manifest-1", "c1", "saka", manifest))
	p.dispatch(context.Background(), r.handle, in("chunk-1", "c1", "saka", chunk))
	if got := len(r.all()); got != 0 {
		t.Fatalf("incomplete roster parts reached model: got %d messages, want 0", got)
	}
}

// 规格: allow_from 可配置 (默认 grok,dot,claude-cloud,hermes)
func TestAllowFromConfigurable(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "x"), in("m2", "c2", "zed", "y")}}
	_, r := startP(t, c, newFakeAcker(), map[string]any{"allow_from": "zed"})
	eventually(t, "zed delivered", func() bool { return len(r.all()) == 1 })
	if r.all()[0].UserID != "zed" {
		t.Fatalf("got %q", r.all()[0].UserID)
	}
}

func TestDefaultAllowFrom(t *testing.T) {
	c := &fakeClient{}
	p := newPlatform(c, newFakeAcker(), map[string]any{"poll_interval": "10ms"})
	c.pending = []Inbound{in("a", "c1", "hermes", "x"), in("b", "c2", "claude-cloud", "y"), in("c", "c3", "nobody", "z")}
	r := &recorder{}
	if err := p.Start(r.handle); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	eventually(t, "two delivered", func() bool { return len(r.all()) == 2 })
}

// 规格: 用 MessageID 去重防止并发重入 (回合未完成时消息在 read --pending 中反复出现)
func TestDedupByMessageID(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "hello")}}
	_, r := startP(t, c, newFakeAcker(), nil)
	eventually(t, "polled many times", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.readHits >= 5
	})
	if n := len(r.all()); n != 1 {
		t.Fatalf("delivered %d times, want 1", n)
	}
}

// 规格: 契约 Acker.Track — 消息交给 engine 时登记
func TestTrackOnDispatch(t *testing.T) {
	ak := newFakeAcker()
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "hello")}}
	_, r := startP(t, c, ak, nil)
	eventually(t, "delivered", func() bool { return len(r.all()) == 1 })
	ak.mu.Lock()
	defer ak.mu.Unlock()
	if got := ak.tracked["a2a:c1"]; len(got) != 1 || got[0] != "m1" {
		t.Fatalf("tracked=%v", ak.tracked)
	}
}

func replyCtxOf(t *testing.T, r *recorder) any {
	t.Helper()
	eventually(t, "delivered", func() bool { return len(r.all()) >= 1 })
	return r.all()[0].ReplyCtx
}

// 规格: 回合最终回复成功 -> TurnSucceeded; 失败 -> TurnFailed 且不 ack
func TestReplySuccessCallsTurnSucceeded(t *testing.T) {
	ak := newFakeAcker()
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, ak, nil)
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, "answer"); err != nil {
		t.Fatal(err)
	}
	p.FinishTurn(context.Background(), rc, true)
	sent := c.sentCopy()
	if len(sent) != 1 || sent[0].ReplyTo != "m1" || sent[0].Text != "answer" {
		t.Fatalf("sent=%+v", sent)
	}
	ak.mu.Lock()
	defer ak.mu.Unlock()
	if len(ak.ok) != 1 || ak.ok[0] != "m1" || len(ak.failed) != 0 {
		t.Fatalf("ok=%v failed=%v", ak.ok, ak.failed)
	}
}

func TestReplyFailureCallsTurnFailedNoAck(t *testing.T) {
	ak := newFakeAcker()
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	c.setSendErr(errBoom)
	p, r := startP(t, c, ak, nil)
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, "answer"); err == nil {
		t.Fatal("want error")
	}
	p.FinishTurn(context.Background(), rc, false)
	ak.mu.Lock()
	defer ak.mu.Unlock()
	if len(ak.failed) != 1 || len(ak.ok) != 0 {
		t.Fatalf("ok=%v failed=%v", ak.ok, ak.failed)
	}
	if len(c.ackedCopy()) != 0 {
		t.Fatalf("acked on failure: %v", c.ackedCopy())
	}
}

// 规格: 成功后才 ack — 真实 L2 Acker 端到端
func TestAckOnlyAfterSuccessfulReply(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, newL2Acker(c, 3), nil)
	rc := replyCtxOf(t, r)
	if len(c.ackedCopy()) != 0 {
		t.Fatalf("acked before reply: %v", c.ackedCopy())
	}
	if err := p.Reply(context.Background(), rc, "done"); err != nil {
		t.Fatal(err)
	}
	if len(c.ackedCopy()) != 0 {
		t.Fatalf("acked by Reply before FinishTurn: %v", c.ackedCopy())
	}
	p.FinishTurn(context.Background(), rc, true)
	eventually(t, "m1 acked", func() bool { a := c.ackedCopy(); return len(a) == 1 && a[0] == "m1" })
}

// 规格: 回复失败不 ack, 下轮 read --pending 再次出现 (重投); 重投后成功才 ack
func TestFailedReplyIsRedeliveredThenAcked(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	c.setSendErr(errBoom)
	p, r := startP(t, c, newL2Acker(c, 5), nil)
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, "a"); err == nil {
		t.Fatal("want error")
	}
	p.FinishTurn(context.Background(), rc, false)
	if len(c.ackedCopy()) != 0 {
		t.Fatalf("acked after failure: %v", c.ackedCopy())
	}
	// redelivered to the engine (dedup slot released after failure)
	eventually(t, "redelivered", func() bool { return len(r.all()) >= 2 })
	c.setSendErr(nil)
	rc2 := r.all()[1].ReplyCtx
	if err := p.Reply(context.Background(), rc2, "a"); err != nil {
		t.Fatal(err)
	}
	p.FinishTurn(context.Background(), rc2, true)
	eventually(t, "acked after success", func() bool { return len(c.ackedCopy()) == 1 })
}

// 规格: 超过 N 次后告警并 ack, 防止死循环
func TestRetryExhaustionAcks(t *testing.T) {
	c := &fakeClient{}
	ak := newL2Acker(c, 2)
	// each redelivery is handed to the engine again, so it is tracked again
	for i := 0; i < 2; i++ {
		ak.Track("a2a:c1", []string{"m1"})
		ak.TurnFailedIDs("a2a:c1", []string{"m1"}, errBoom)
	}
	eventually(t, "acked after exhausting retries", func() bool { return len(c.ackedCopy()) == 1 })
}

// 规格: 多条消息覆盖同一回合, 成功后统一 ack
func TestTurnSucceededAcksAllTracked(t *testing.T) {
	c := &fakeClient{}
	ak := newL2Acker(c, 3)
	ak.Track("a2a:c1", []string{"m1", "m2"})
	ak.TurnSucceededIDs([]string{"m1", "m2"})
	eventually(t, "both acked", func() bool { return len(c.ackedCopy()) == 2 })
}

// 规格: pong — claude-cloud 的 nibiru.pong 探测在平台层回 pong 并 ack, 不进模型
const pingBody = `{"type":"nibiru.ping.v1","request_id":"0123456789abcdef","from":"claude-cloud","sent_at":1760000000}`

func TestPingFromCloudRepliesPongAndSkipsModel(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("p1", "c9", "claude-cloud", pingBody)}}
	_, r := startP(t, c, newL2Acker(c, 3), nil)
	eventually(t, "pong sent", func() bool { return len(c.sentCopy()) == 1 })
	s := c.sentCopy()[0]
	if s.ReplyTo != "p1" || s.Key != "pong-0123456789abcdef" ||
		!strings.Contains(s.Text, `"type":"nibiru.pong.v1"`) || !strings.Contains(s.Text, `"request_id":"0123456789abcdef"`) {
		t.Fatalf("bad pong: %+v", s)
	}
	eventually(t, "ping acked", func() bool { a := c.ackedCopy(); return len(a) == 1 && a[0] == "p1" })
	if n := len(r.all()); n != 0 {
		t.Fatalf("ping reached model: %d", n)
	}
}

// 规格: 其他发件人的 ping 只 ack, 不回复, 不进模型
func TestPingFromOthersOnlyAcked(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("p1", "c9", "dot", pingBody)}}
	_, r := startP(t, c, newL2Acker(c, 3), nil)
	eventually(t, "acked", func() bool { return len(c.ackedCopy()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if len(c.sentCopy()) != 0 || len(r.all()) != 0 {
		t.Fatalf("sent=%v delivered=%d", c.sentCopy(), len(r.all()))
	}
}

// 规格: Reply/Send 分块 — max_chunk 默认 6000 字节, rune 边界, 前缀 [i/n sha256:<全文前12位>], 键 <message_id>-<i>
func TestSpecChunking(t *testing.T) {
	text := strings.Repeat("你好世界", 1200) // 4800 runes = 14400 bytes
	sum := sha256.Sum256([]byte(text))
	want := hex.EncodeToString(sum[:])[:12]
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, newFakeAcker(), nil)
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, text); err != nil {
		t.Fatal(err)
	}
	sent := c.sentCopy()
	if len(sent) < 3 {
		t.Fatalf("want >=3 chunks, got %d", len(sent))
	}
	n := len(sent)
	var rebuilt strings.Builder
	for i, s := range sent {
		prefix := fmt.Sprintf("[%d/%d sha256:%s]", i+1, n, want)
		if !strings.HasPrefix(s.Text, prefix) {
			t.Fatalf("chunk %d prefix: %.40q want %s", i, s.Text, prefix)
		}
		body := strings.TrimPrefix(strings.TrimPrefix(s.Text, prefix), "\n")
		body = strings.TrimPrefix(body, " ")
		if !utf8.ValidString(body) {
			t.Fatalf("chunk %d splits a rune", i)
		}
		if len(body) > 6000 {
			t.Fatalf("chunk %d body %d bytes > 6000", i, len(body))
		}
		if s.Key != fmt.Sprintf("m1-%d", i+1) {
			t.Fatalf("chunk %d key=%q", i, s.Key)
		}
		if s.ReplyTo != "m1" {
			t.Fatalf("replyTo=%q", s.ReplyTo)
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != text {
		t.Fatal("reassembled text differs")
	}
}

// 短文本不分块、不加前缀; 键仍可预测
func TestShortReplyNotChunked(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, newFakeAcker(), nil)
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, "short"); err != nil {
		t.Fatal(err)
	}
	sent := c.sentCopy()
	if len(sent) != 1 || sent[0].Text != "short" {
		t.Fatalf("sent=%+v", sent)
	}
}

// max_chunk 可配
func TestMaxChunkConfigurable(t *testing.T) {
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, newFakeAcker(), map[string]any{"max_chunk": 100})
	rc := replyCtxOf(t, r)
	if err := p.Reply(context.Background(), rc, strings.Repeat("a", 250)); err != nil {
		t.Fatal(err)
	}
	if n := len(c.sentCopy()); n < 3 {
		t.Fatalf("chunks=%d", n)
	}
}

// 规格: 分块发送中途失败 -> 返回错误，引擎随后 FinishTurn(false) -> TurnFailed (不 ack)
func TestChunkFailureMidwayIsError(t *testing.T) {
	ak := newFakeAcker()
	c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "q")}}
	p, r := startP(t, c, ak, map[string]any{"max_chunk": 50})
	rc := replyCtxOf(t, r)
	c.setSendErr(errBoom)
	if err := p.Reply(context.Background(), rc, strings.Repeat("a", 200)); err == nil {
		t.Fatal("want error")
	}
	p.FinishTurn(context.Background(), rc, false)
	ak.mu.Lock()
	defer ak.mu.Unlock()
	if len(ak.ok) != 0 || len(ak.failed) != 1 {
		t.Fatalf("ok=%v failed=%v", ak.ok, ak.failed)
	}
}

// 规格: Stop 必须让轮询 goroutine 退出
func TestSpecStopEndsPolling(t *testing.T) {
	c := &fakeClient{}
	p := newTestPlatform(t, c, newFakeAcker(), nil)
	if err := p.Start((&recorder{}).handle); err != nil {
		t.Fatal(err)
	}
	eventually(t, "polling", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.readHits >= 2 })
	done := make(chan struct{})
	go func() { _ = p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	c.mu.Lock()
	h := c.readHits
	c.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readHits != h {
		t.Fatalf("still polling after Stop: %d -> %d", h, c.readHits)
	}
}

// ReadPending 出错不应使轮询退出
func TestReadErrorKeepsPolling(t *testing.T) {
	c := &fakeClient{readErr: errBoom, pending: []Inbound{in("m1", "c1", "grok", "x")}}
	_, r := startP(t, c, newFakeAcker(), nil)
	time.Sleep(40 * time.Millisecond)
	c.mu.Lock()
	c.readErr = nil
	c.mu.Unlock()
	eventually(t, "recovered", func() bool { return len(r.all()) == 1 })
}

// 规格: 按本轮 message IDs 结算 — m2 在 m1 回合结束前已入队同一 session,
// m1 的 FinishTurn(成功/失败) 都不得 ACK 或记失败 m2
func TestFinishTurnDoesNotSettleQueuedNextMessage(t *testing.T) {
	for _, ok := range []bool{true, false} {
		c := &fakeClient{pending: []Inbound{in("m1", "c1", "grok", "a"), in("m2", "c1", "grok", "b")}}
		ak := newL2Acker(c, 3)
		p, r := startP(t, c, ak, nil)
		eventually(t, "both dispatched", func() bool { return len(r.all()) == 2 })
		p.FinishTurn(context.Background(), r.all()[0].ReplyCtx, ok)
		if ok {
			eventually(t, "m1 acked", func() bool { return len(c.ackedCopy()) == 1 })
		}
		time.Sleep(50 * time.Millisecond)
		for _, id := range c.ackedCopy() {
			if id == "m2" {
				t.Fatalf("ok=%v: m2 acked by m1's turn", ok)
			}
		}
		if ak.Admit("m2") {
			t.Fatalf("ok=%v: m2 must remain in flight", ok)
		}
		p.FinishTurn(context.Background(), r.all()[1].ReplyCtx, true)
		eventually(t, "m2 acked by own turn", func() bool {
			for _, id := range c.ackedCopy() {
				if id == "m2" {
					return true
				}
			}
			return false
		})
	}
}
