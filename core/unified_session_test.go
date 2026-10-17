package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- test doubles (nibiru#966) ---

type usPlatform struct {
	n    string
	mu   sync.Mutex
	outs []usOut
}

type usCronPlatform struct {
	*usPlatform
	muRoutes sync.Mutex
	routes   []string
}

func (p *usCronPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	p.muRoutes.Lock()
	p.routes = append(p.routes, sessionKey)
	p.muRoutes.Unlock()
	return sessionKey, nil
}

func (p *usCronPlatform) reconstructedRoutes() []string {
	p.muRoutes.Lock()
	defer p.muRoutes.Unlock()
	return append([]string(nil), p.routes...)
}

type usOut struct {
	ctx     any
	content string
}

func (p *usPlatform) Name() string               { return p.n }
func (p *usPlatform) Start(MessageHandler) error { return nil }
func (p *usPlatform) Stop() error                { return nil }
func (p *usPlatform) rec(ctx any, c string) {
	p.mu.Lock()
	p.outs = append(p.outs, usOut{ctx, c})
	p.mu.Unlock()
}
func (p *usPlatform) Reply(_ context.Context, ctx any, c string) error { p.rec(ctx, c); return nil }
func (p *usPlatform) Send(_ context.Context, ctx any, c string) error  { p.rec(ctx, c); return nil }
func (p *usPlatform) outsFor(ctx any) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var r []string
	for _, o := range p.outs {
		if o.ctx == ctx {
			r = append(r, o.content)
		}
	}
	return r
}

// usSession answers each Send only after the test releases it, so the test can
// observe that turns are strictly serial.
type usSession struct {
	events  chan Event
	release chan struct{}
	mu      sync.Mutex
	prompts []string
}

func (s *usSession) Send(prompt string, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	n := len(s.prompts)
	s.mu.Unlock()
	go func() {
		<-s.release
		s.events <- Event{Type: EventResult, Content: "answer-" + string(rune('0'+n)), Done: true}
	}()
	return nil
}
func (s *usSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *usSession) Events() <-chan Event                             { return s.events }
func (s *usSession) CurrentSessionID() string                         { return "us-session" }
func (s *usSession) Alive() bool                                      { return true }
func (s *usSession) Close() error                                     { return nil }
func (s *usSession) promptList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

type usAgent struct {
	mu     sync.Mutex
	starts int
	sess   *usSession
}

func (a *usAgent) Name() string { return "us" }
func (a *usAgent) StartSession(context.Context, string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	return a.sess, nil
}
func (a *usAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *usAgent) Stop() error                                              { return nil }
func (a *usAgent) startCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}

func usWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newUSEngine(unified bool) (*Engine, *usAgent, *usPlatform, *usPlatform, *usSession) {
	sess := &usSession{events: make(chan Event, 8), release: make(chan struct{}, 8)}
	ag := &usAgent{sess: sess}
	a2a := &usPlatform{n: "a2a"}
	ding := &usPlatform{n: "dingtalk"}
	e := NewEngine("us", ag, []Platform{a2a, ding}, "", LangEnglish)
	e.SetUnifiedSession(unified)
	return e, ag, a2a, ding, sess
}

// Unified: two A2A conversations + one DingTalk message -> one agent session,
// strictly serial, each reply routed back to its own platform/reply ctx.
func TestUnifiedSession_ThreeSourcesOneAgentSessionSerial(t *testing.T) {
	e, ag, a2a, ding, sess := newUSEngine(true)

	e.handleMessage(a2a, &Message{SessionKey: "a2a:c1", Platform: "a2a", MessageID: "am-1", UserID: "dot", UserName: "dot", Content: "hello from c1", ReplyCtx: "ctx-c1"})
	usWait(t, "first prompt", func() bool { return len(sess.promptList()) == 1 })
	e.handleMessage(a2a, &Message{SessionKey: "a2a:c2", Platform: "a2a", MessageID: "am-2", UserID: "grok", UserName: "grok", Content: "hello from c2", ReplyCtx: "ctx-c2"})
	e.handleMessage(ding, &Message{SessionKey: "dingtalk:cid9:u7", Platform: "dingtalk", MessageID: "dm-3", UserID: "u7", UserName: "Nick", Content: "hello from ding", ReplyCtx: "ctx-ding"})

	time.Sleep(100 * time.Millisecond)
	if n := len(sess.promptList()); n != 1 {
		t.Fatalf("turns must be serial: %d prompts sent while first turn busy", n)
	}

	sess.release <- struct{}{}
	usWait(t, "second prompt", func() bool { return len(sess.promptList()) == 2 })
	sess.release <- struct{}{}
	usWait(t, "third prompt", func() bool { return len(sess.promptList()) == 3 })
	sess.release <- struct{}{}
	usWait(t, "replies", func() bool {
		return strings.Contains(joinOuts(a2a.outsFor("ctx-c1")), "answer-1") &&
			strings.Contains(joinOuts(a2a.outsFor("ctx-c2")), "answer-2") &&
			strings.Contains(joinOuts(ding.outsFor("ctx-ding")), "answer-3")
	})

	if n := ag.startCount(); n != 1 {
		t.Fatalf("agent sessions started = %d, want 1", n)
	}
	ps := sess.promptList()
	wants := []struct{ sub []string }{
		{[]string{"platform=a2a", "sender_id=dot", "conversation=a2a:c1", "msg_id=am-1", "hello from c1"}},
		{[]string{"platform=a2a", "sender_id=grok", "conversation=a2a:c2", "msg_id=am-2", "hello from c2"}},
		{[]string{"platform=dingtalk", "sender_id=u7", "sender_name=\"Nick\"", "conversation=dingtalk:cid9:u7", "msg_id=dm-3", "hello from ding"}},
	}
	for i, w := range wants {
		for _, s := range w.sub {
			if !strings.Contains(ps[i], s) {
				t.Errorf("prompt[%d] missing %q: %q", i, s, ps[i])
			}
		}
	}
	// Each reply landed only on its own source, in answer order.
	if got := joinOuts(a2a.outsFor("ctx-c1")); !strings.Contains(got, "answer-1") || strings.Contains(got, "answer-2") {
		t.Errorf("c1 replies = %q", got)
	}
	if got := joinOuts(a2a.outsFor("ctx-c2")); !strings.Contains(got, "answer-2") {
		t.Errorf("c2 replies = %q", got)
	}
	if got := joinOuts(ding.outsFor("ctx-ding")); !strings.Contains(got, "answer-3") {
		t.Errorf("ding replies = %q", got)
	}
}

func joinOuts(o []string) string { return strings.Join(o, "\n") }

// Default (off): original per-key behaviour, no source tag.
func TestUnifiedSession_OffKeepsPerKeySessions(t *testing.T) {
	e, ag, a2a, _, sess := newUSEngine(false)
	e.handleMessage(a2a, &Message{SessionKey: "a2a:c1", Platform: "a2a", MessageID: "am-1", UserID: "dot", Content: "plain", ReplyCtx: "ctx-c1"})
	usWait(t, "prompt", func() bool { return len(sess.promptList()) == 1 })
	if strings.Contains(sess.promptList()[0], "conversation=") {
		t.Fatalf("source tag injected while unified_session is off: %q", sess.promptList()[0])
	}
	e.interactiveMu.Lock()
	_, ok := e.interactiveStates["a2a:c1"]
	e.interactiveMu.Unlock()
	if !ok {
		t.Fatalf("expected per-key interactive state when off")
	}
	_ = ag
}

// Unified key is stable and derived from the project name.
func TestUnifiedSession_KeyStable(t *testing.T) {
	e, _, a2a, _, sess := newUSEngine(true)
	e.handleMessage(a2a, &Message{SessionKey: "a2a:c1", Platform: "a2a", MessageID: "am-1", UserID: "dot", Content: "x", ReplyCtx: "ctx-c1"})
	usWait(t, "prompt", func() bool { return len(sess.promptList()) == 1 })
	e.interactiveMu.Lock()
	_, ok := e.interactiveStates[UnifiedSessionKey("us")]
	n := len(e.interactiveStates)
	e.interactiveMu.Unlock()
	if !ok || n != 1 {
		t.Fatalf("want exactly the unified key state, ok=%v n=%d", ok, n)
	}
}

// A slash command arriving on any source must not get the tag prefix.
func TestUnifiedSession_CommandNotTagged(t *testing.T) {
	e, _, a2a, _, _ := newUSEngine(true)
	msg := &Message{SessionKey: "a2a:c1", Platform: "a2a", MessageID: "am-1", UserID: "dot", Content: "/help", ReplyCtx: "ctx-c1"}
	e.handleMessage(a2a, msg)
	if len(a2a.outsFor("ctx-c1")) == 0 {
		t.Fatalf("/help got no reply; command routing broken under unified session")
	}
}

func TestUnifiedSession_CronCLIEnvironmentKeepsUnifiedIdentityWithoutStaticRoute(t *testing.T) {
	agent := &sessionEnvRecordingAgent{}
	a2a := &usPlatform{n: "a2a"}
	e := NewEngine("sakura", agent, []Platform{a2a}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetUnifiedSession(true)

	e.handleMessage(a2a, &Message{
		SessionKey: "a2a:c1", Platform: "a2a", UserID: "admin",
		Content: "create a cron job", ReplyCtx: "ctx-a2a",
	})

	usWait(t, "agent session environment", func() bool {
		return agent.EnvValue("CC_SESSION_KEY") != ""
	})
	if got := agent.EnvValue("CC_SESSION_KEY"); got != UnifiedSessionKey("sakura") {
		t.Fatalf("CC_SESSION_KEY = %q, want unified identity %q", got, UnifiedSessionKey("sakura"))
	}
	if got := agent.EnvValue("CC_ROUTE_SESSION_KEY"); got != "" {
		t.Fatalf("CC_ROUTE_SESSION_KEY = %q, want no static route from the session's first platform", got)
	}
}

func TestUnifiedSession_CronCreationPersistsDeliveryRoute(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewCronScheduler(store)
	a2a := &usPlatform{n: "a2a"}
	ding := &usPlatform{n: "dingtalk"}
	e := NewEngine("sakura", &stubAgent{}, []Platform{a2a, ding}, "", LangEnglish)
	defer e.cancel()
	e.SetUnifiedSession(true)
	e.SetAdminFrom("admin")
	e.cronScheduler = scheduler

	e.handleMessage(a2a, &Message{
		SessionKey: "a2a:c1", Platform: "a2a", UserID: "admin",
		Content: "/cron add 0 6 * * * prepare report", ReplyCtx: "ctx-a2a",
	})
	e.handleMessage(ding, &Message{
		SessionKey: "dingtalk:cid9:u7", Platform: "dingtalk", UserID: "admin",
		Content: "/cron addexec 0 7 * * * echo ready", ReplyCtx: "ctx-ding",
	})

	jobs := store.List()
	if len(jobs) != 2 {
		t.Fatalf("created cron jobs = %d, want 2", len(jobs))
	}
	wantRoutes := map[string]string{
		"prepare report": "a2a:c1",
		"echo ready":     "dingtalk:cid9:u7",
	}
	data, err := os.ReadFile(filepath.Join(dir, "crons", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		wantRoute, ok := wantRoutes[job.Prompt]
		if !ok {
			wantRoute, ok = wantRoutes[job.Exec]
		}
		if !ok {
			t.Fatalf("unexpected cron job prompt=%q exec=%q", job.Prompt, job.Exec)
		}
		if got := persistedRouteForID(t, persisted, job.ID); got != wantRoute {
			t.Errorf("persisted route for %q = %q, want %q", job.ID, got, wantRoute)
		}
	}
	if jobs[0].SessionKey != UnifiedSessionKey("sakura") || jobs[1].SessionKey != UnifiedSessionKey("sakura") {
		t.Fatalf("cron execution identity keys = %q, %q; want unified identity %q", jobs[0].SessionKey, jobs[1].SessionKey, UnifiedSessionKey("sakura"))
	}
}

func TestUnifiedSession_CronRouteSurvivesRestartAndExecutesOnUnifiedSession(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "crons"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `[{"id":"unified-cron","project":"sakura","session_key":"unified:sakura","route_session_key":"a2a:c1","cron_expr":"0 6 * * *","prompt":"scheduled report","enabled":true}]`
	if err := os.WriteFile(filepath.Join(dir, "crons", "jobs.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := store.Get("unified-cron")
	if job == nil {
		t.Fatal("persisted cron job did not load after restart")
	}

	a2a := &usCronPlatform{usPlatform: &usPlatform{n: "a2a"}}
	ding := &usCronPlatform{usPlatform: &usPlatform{n: "dingtalk"}}
	session := newResultAgentSession("report complete")
	e := NewEngine("sakura", &resultAgent{session: session}, []Platform{a2a, ding}, "", LangEnglish)
	defer e.cancel()
	e.SetUnifiedSession(true)
	scheduler := NewCronScheduler(store)
	e.cronScheduler = scheduler
	scheduler.RegisterEngine("sakura", e)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()

	scheduler.mu.RLock()
	_, scheduled := scheduler.entries[job.ID]
	scheduler.mu.RUnlock()
	if !scheduled {
		t.Fatal("persisted unified cron job was not scheduled after restart")
	}
	if err := e.ExecuteCronJob(job); err != nil {
		t.Fatalf("ExecuteCronJob() after restart: %v", err)
	}

	if got := a2a.reconstructedRoutes(); len(got) != 1 || got[0] != "a2a:c1" {
		t.Fatalf("A2A reply-context routes = %v, want [a2a:c1]", got)
	}
	if got := len(a2a.outsFor("a2a:c1")); got == 0 {
		t.Fatalf("A2A route received no cron output")
	}
	if got := len(ding.outsFor("dingtalk:cid9:u7")); got != 0 {
		t.Fatalf("DingTalk received %d outputs, want none", got)
	}
	if got := len(e.sessions.ListSessions(UnifiedSessionKey("sakura"))); got != 1 {
		t.Fatalf("unified execution session count = %d, want 1", got)
	}
	if got := len(e.sessions.ListSessions("a2a:c1")); got != 0 {
		t.Fatalf("route key created %d agent sessions, want none", got)
	}
}

func TestUnifiedSession_ModelAPICronCreateRestartAndExecuteUsesOriginalRoute(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewCronScheduler(store)
	a2a := &usCronPlatform{usPlatform: &usPlatform{n: "a2a"}}
	ding := &usCronPlatform{usPlatform: &usPlatform{n: "dingtalk"}}
	session := newResultAgentSession("report complete")
	e := NewEngine("sakura", &resultAgent{session: session}, []Platform{a2a, ding}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetUnifiedSession(true)
	identity := UnifiedSessionKey("sakura")
	activeSession := e.sessions.GetOrCreateActive(identity)
	activeLockGen, locked := activeSession.TryLock()
	if !locked {
		t.Fatal("failed to lock unified session for active-route fixture")
	}
	t.Cleanup(func() { activeSession.Unlock(activeLockGen) })
	e.interactiveMu.Lock()
	e.interactiveStates[identity] = &interactiveState{
		busySession:                 activeSession,
		currentTurnSessionKey:       identity,
		currentTurnOriginSessionKey: "a2a:c1",
		currentTurnLockGen:          activeLockGen,
	}
	e.interactiveMu.Unlock()

	api, err := NewAPIServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.listener.Close() })
	api.SetCronScheduler(scheduler)
	api.RegisterEngine("sakura", e)

	payload, err := json.Marshal(map[string]any{
		"project": "sakura", "session_key": UnifiedSessionKey("sakura"),
		"route_session_key": "a2a:c1", "cron_expr": "0 6 * * *", "prompt": "scheduled report",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cron/add", strings.NewReader(string(payload)))
	response := httptest.NewRecorder()
	api.mux.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /cron/add status = %d, body %s", response.Code, response.Body.String())
	}

	var created CronJob
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode cron add response: %v", err)
	}
	if created.SessionKey != UnifiedSessionKey("sakura") {
		t.Errorf("created SessionKey = %q, want unified identity %q", created.SessionKey, UnifiedSessionKey("sakura"))
	}
	if created.RouteSessionKey != "a2a:c1" {
		t.Errorf("created RouteSessionKey = %q, want original route a2a:c1", created.RouteSessionKey)
	}
	activeSession.Unlock(activeLockGen)

	// Reload the store to exercise the persisted API-created record, then start
	// a fresh scheduler as a process restart would.
	restartedStore, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := restartedStore.Get(created.ID)
	if job == nil {
		t.Fatal("API-created cron job did not load after restart")
	}
	restartedScheduler := NewCronScheduler(restartedStore)
	e.cronScheduler = restartedScheduler
	restartedScheduler.RegisterEngine("sakura", e)
	if err := restartedScheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restartedScheduler.Stop() })

	restartedScheduler.mu.RLock()
	_, scheduled := restartedScheduler.entries[job.ID]
	restartedScheduler.mu.RUnlock()
	if !scheduled {
		t.Error("API-created unified cron job was not scheduled after restart")
	}
	if err := e.ExecuteCronJob(job); err != nil {
		t.Errorf("ExecuteCronJob() after API create/restart: %v", err)
	} else {
		if got := a2a.reconstructedRoutes(); len(got) == 0 || got[len(got)-1] != "a2a:c1" {
			t.Errorf("A2A reconstructed routes = %v, want final route a2a:c1", got)
		}
		if got := len(a2a.outsFor("a2a:c1")); got == 0 {
			t.Error("A2A route received no output from API-created cron job")
		}
		if got := len(ding.outsFor("dingtalk:cid9:u7")); got != 0 {
			t.Errorf("DingTalk received %d outputs, want none", got)
		}
	}
}

func TestCronAddRejectsRouteOnUnconfiguredProjectPlatform(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.listener.Close() })
	api.SetCronScheduler(NewCronScheduler(store))
	e := NewEngine("sakura", &stubAgent{}, []Platform{&usPlatform{n: "a2a"}}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetUnifiedSession(true)
	identity := UnifiedSessionKey("sakura")
	session := e.sessions.GetOrCreateActive(identity)
	lockGen, locked := session.TryLock()
	if !locked {
		t.Fatal("failed to lock unified session for active-route fixture")
	}
	t.Cleanup(func() { session.Unlock(lockGen) })
	e.interactiveMu.Lock()
	e.interactiveStates[identity] = &interactiveState{
		busySession:                 session,
		currentTurnSessionKey:       identity,
		currentTurnOriginSessionKey: "other-project-platform:c1",
		currentTurnLockGen:          lockGen,
	}
	e.interactiveMu.Unlock()
	api.RegisterEngine("sakura", e)

	payload, err := json.Marshal(map[string]any{
		"project": "sakura", "session_key": UnifiedSessionKey("sakura"),
		"route_session_key": "a2a:c1", "cron_expr": "0 6 * * *", "prompt": "must reject",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cron/add", strings.NewReader(string(payload)))
	response := httptest.NewRecorder()
	api.mux.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST /cron/add with an unconfigured route status = %d, want %d; body: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if got := len(store.List()); got != 0 {
		t.Fatalf("unconfigured route persisted %d cron jobs, want none", got)
	}
}

func TestUnifiedSession_ModelAPICronUsesCurrentQueuedTurnRouteAfterRestart(t *testing.T) {
	session := &usSession{events: make(chan Event, 8), release: make(chan struct{}, 8)}
	ag := &usAgent{sess: session}
	a2a := &usCronPlatform{usPlatform: &usPlatform{n: "a2a"}}
	ding := &usCronPlatform{usPlatform: &usPlatform{n: "dingtalk"}}
	e := NewEngine("us", ag, []Platform{a2a, ding}, "", LangEnglish)
	e.SetUnifiedSession(true)
	defer func() {
		// Release any Send waiting on the test-controlled agent before stopping.
		for range 3 {
			session.release <- struct{}{}
		}
		_ = e.Stop()
	}()

	cronDir := t.TempDir()
	store, err := NewCronStore(cronDir)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewCronScheduler(store)
	api, err := NewAPIServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer api.listener.Close()
	api.SetCronScheduler(scheduler)
	api.RegisterEngine("us", e)

	// The first A2A turn starts the shared process; a DingTalk message arrives
	// while it is busy and is processed next by the same unified session.
	e.handleMessage(a2a, &Message{
		SessionKey: "a2a:c1", Platform: "a2a", MessageID: "am-first",
		UserID: "u1", UserName: "A2A", Content: "first turn", ReplyCtx: "ctx-a2a",
	})
	usWait(t, "first A2A prompt", func() bool { return len(session.promptList()) == 1 })
	e.handleMessage(ding, &Message{
		SessionKey: "dingtalk:cid9:u7", Platform: "dingtalk", MessageID: "dm-second",
		UserID: "u7", UserName: "DingTalk", Content: "create a scheduled reminder", ReplyCtx: "ctx-ding",
	})
	session.release <- struct{}{}
	usWait(t, "queued DingTalk turn on the same agent session", func() bool { return len(session.promptList()) == 2 })
	if got := ag.startCount(); got != 1 {
		t.Fatalf("agent sessions started before cron API call = %d, want one shared process", got)
	}

	// This stale body route represents the route inherited from the process's
	// initial A2A turn. The API must derive the live route from the Engine's
	// trusted current turn, not let the body select a configured platform.
	payload, err := json.Marshal(map[string]any{
		"project": "us", "session_key": UnifiedSessionKey("us"),
		"route_session_key": "a2a:c1", "cron_expr": "0 6 * * *", "prompt": "scheduled reminder",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cron/add", strings.NewReader(string(payload)))
	response := httptest.NewRecorder()
	api.mux.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /cron/add status = %d, body %s", response.Code, response.Body.String())
	}
	var created CronJob
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode cron response: %v", err)
	}
	if created.SessionKey != UnifiedSessionKey("us") {
		t.Fatalf("cron identity = %q, want %q", created.SessionKey, UnifiedSessionKey("us"))
	}
	if created.RouteSessionKey != "dingtalk:cid9:u7" {
		t.Fatalf("cron route = %q, want current DingTalk turn route dingtalk:cid9:u7", created.RouteSessionKey)
	}

	session.release <- struct{}{}
	usWait(t, "DingTalk turn reply", func() bool { return len(ding.outsFor("ctx-ding")) > 0 })
	if got := ag.startCount(); got != 1 {
		t.Fatalf("agent sessions started after queued turn = %d, want one shared process", got)
	}
	usWait(t, "DingTalk turn lock release", func() bool {
		return !e.sessions.GetOrCreateActive(UnifiedSessionKey("us")).Busy()
	})

	// Reload the API-created job and start a fresh scheduler, as after restart.
	restartedStore, err := NewCronStore(cronDir)
	if err != nil {
		t.Fatal(err)
	}
	job := restartedStore.Get(created.ID)
	if job == nil {
		t.Fatal("API-created cron job did not load after restart")
	}
	restartedScheduler := NewCronScheduler(restartedStore)
	e.cronScheduler = restartedScheduler
	restartedScheduler.RegisterEngine("us", e)
	if err := restartedScheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer restartedScheduler.Stop()
	restartedScheduler.mu.RLock()
	_, scheduled := restartedScheduler.entries[job.ID]
	restartedScheduler.mu.RUnlock()
	if !scheduled {
		t.Fatal("DingTalk-routed unified cron job was not scheduled after restart")
	}

	execDone := make(chan error, 1)
	go func() { execDone <- e.ExecuteCronJob(job) }()
	promptDeadline := time.After(5 * time.Second)
	for len(session.promptList()) < 3 {
		select {
		case err := <-execDone:
			t.Fatalf("ExecuteCronJob() returned before sending the cron prompt: %v", err)
		case <-promptDeadline:
			t.Fatal("timed out waiting for cron agent prompt")
		case <-time.After(10 * time.Millisecond):
		}
	}
	session.release <- struct{}{}
	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("ExecuteCronJob() after API create/restart: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for restarted cron execution")
	}
	if got := len(ding.outsFor("dingtalk:cid9:u7")); got == 0 {
		t.Fatal("DingTalk route received no cron output after restart")
	}
	if got := len(a2a.outsFor("a2a:c1")); got != 0 {
		t.Fatalf("A2A route received %d cron outputs, want none", got)
	}
}

func TestUnifiedSession_ModelAPICronRejectsCallerRouteWithoutActiveTurn(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer api.listener.Close()
	api.SetCronScheduler(NewCronScheduler(store))
	e := NewEngine("us", &stubAgent{}, []Platform{&usPlatform{n: "a2a"}, &usPlatform{n: "dingtalk"}}, "", LangEnglish)
	defer e.Stop()
	e.SetUnifiedSession(true)
	api.RegisterEngine("us", e)

	payload, err := json.Marshal(map[string]any{
		"project": "us", "session_key": UnifiedSessionKey("us"),
		"route_session_key": "a2a:c1", "cron_expr": "0 6 * * *", "prompt": "must fail closed",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cron/add", strings.NewReader(string(payload)))
	response := httptest.NewRecorder()
	api.mux.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST /cron/add without an active turn status = %d, want %d; body: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if got := len(store.List()); got != 0 {
		t.Fatalf("caller-selected route persisted %d cron jobs without an active turn, want none", got)
	}
}

func persistedRouteForID(t *testing.T, jobs []map[string]any, id string) string {
	t.Helper()
	for _, job := range jobs {
		if job["id"] == id {
			route, _ := job["route_session_key"].(string)
			return route
		}
	}
	t.Fatalf("cron job %q was not persisted", id)
	return ""
}
