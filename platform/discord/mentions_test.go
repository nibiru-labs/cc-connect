package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/chenhg5/cc-connect/core"
)

// All IDs and text are synthetic; every REST request goes to the local fake server.
func TestOutgoingTextExplicitlySuppressesMentions(t *testing.T) {
	const text = "untrusted @everyone <@123456789> <@&987654321>"
	interaction := func() *interactionReplyCtx {
		return &interactionReplyCtx{channelID: "ch-1", interaction: &discordgo.Interaction{AppID: "app-1", Token: "synthetic-interaction"}}
	}
	buttons := [][]core.ButtonOption{{{Text: "Continue", Data: "cmd:/help"}}}
	cases := []struct {
		name        string
		failWebhook bool
		requests    int
		send        func(*Platform) error
	}{
		{name: "send", requests: 1, send: func(p *Platform) error { return p.Send(context.Background(), replyContext{channelID: "ch-1"}, text) }},
		{name: "reply", requests: 1, send: func(p *Platform) error {
			return p.Reply(context.Background(), replyContext{channelID: "ch-1", messageID: "in-1"}, text)
		}},
		{name: "thread", requests: 1, send: func(p *Platform) error {
			return p.Reply(context.Background(), replyContext{channelID: "ch-1", threadID: "ch-1", messageID: "in-1"}, text)
		}},
		{name: "interaction_edit", requests: 1, send: func(p *Platform) error { return p.Reply(context.Background(), interaction(), text) }},
		{name: "interaction_followup", requests: 1, send: func(p *Platform) error {
			rc := interaction()
			rc.firstDone = true
			return p.Reply(context.Background(), rc, text)
		}},
		{name: "interaction_fallback", requests: 2, failWebhook: true, send: func(p *Platform) error { return p.Reply(context.Background(), interaction(), text) }},
		{name: "buttons", requests: 1, send: func(p *Platform) error {
			return p.SendWithButtons(context.Background(), replyContext{channelID: "ch-1"}, text, buttons)
		}},
		{name: "interaction_buttons", requests: 2, send: func(p *Platform) error { return p.SendWithButtons(context.Background(), interaction(), text, buttons) }},
		{name: "preview_create", requests: 1, send: func(p *Platform) error {
			_, err := p.SendPreviewStart(context.Background(), replyContext{channelID: "ch-1"}, text)
			return err
		}},
		{name: "preview_edit", requests: 1, send: func(p *Platform) error {
			return p.UpdateMessage(context.Background(), &discordPreviewHandle{channelID: "ch-1", messageID: "out-1"}, text)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []map[string]any
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				bodies = append(bodies, body)
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if tc.failWebhook && strings.Contains(r.URL.Path, "/webhooks/") {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code":50035,"message":"synthetic failure"}`)
					return
				}
				fmt.Fprint(w, `{"id":"out-1","channel_id":"ch-1"}`)
			}))
			defer server.Close()
			p := &Platform{session: newTestDiscordSession(t, server)}
			if err := tc.send(p); err != nil {
				t.Fatal(err)
			}
			if len(bodies) != tc.requests {
				t.Fatalf("requests=%d, want %d", len(bodies), tc.requests)
			}
			for _, body := range bodies {
				assertNoMentions(t, body)
				if body["content"] != text {
					t.Errorf("content changed: %#v", body["content"])
				}
			}
			if tc.name == "thread" {
				if _, exists := bodies[0]["message_reference"]; exists {
					t.Fatal("thread reply must not reference a parent message")
				}
				if !strings.Contains(paths[0], "/channels/ch-1/messages") {
					t.Fatalf("wrong thread route: %s", paths[0])
				}
			}
			if tc.name == "reply" {
				ref, ok := bodies[0]["message_reference"].(map[string]any)
				if !ok || ref["message_id"] != "in-1" {
					t.Fatalf("reply reference lost: %#v", bodies[0])
				}
			}
		})
	}
}

func assertNoMentions(t *testing.T, payload map[string]any) {
	t.Helper()
	mentions, ok := payload["allowed_mentions"].(map[string]any)
	if !ok {
		t.Errorf("missing explicit allowed_mentions")
		return
	}
	parse, ok := mentions["parse"].([]any)
	if !ok || len(parse) != 0 {
		t.Errorf("parse=%#v, want []", mentions["parse"])
	}
	if mentions["replied_user"] != false {
		t.Errorf("replied_user=%#v, want false", mentions["replied_user"])
	}
	for _, name := range []string{"users", "roles"} {
		if value, exists := mentions[name]; exists && value != nil {
			t.Errorf("unexpected %s allowlist: %#v", name, value)
		}
	}
}

func TestOutgoingSplitAndProgressPayloadsSuppressMentions(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"out-1","channel_id":"ch-1"}`)
	}))
	defer server.Close()
	p := &Platform{session: newTestDiscordSession(t, server)}
	text := strings.Repeat("x", maxDiscordLen+20) + " @everyone <@123456789>"
	if err := p.Send(context.Background(), replyContext{channelID: "ch-1"}, text); err != nil {
		t.Fatal(err)
	}
	if len(bodies) < 2 {
		t.Fatal("expected multiple chunks")
	}
	var joined strings.Builder
	for _, body := range bodies {
		assertNoMentions(t, body)
		joined.WriteString(body["content"].(string))
	}
	if joined.String() != text {
		t.Fatal("split content changed")
	}
	progress := core.BuildProgressCardPayloadV2([]core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: "untrusted @everyone"}}, false, "Codex", core.LangEnglish, core.ProgressCardStateRunning)
	for _, payload := range []any{buildDiscordPreviewMessage(progress), buildDiscordPreviewEdit("ch-1", "out-1", progress)} {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err = json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		assertNoMentions(t, body)
	}
}

func TestComponentUpdateExplicitlySuppressesMentions(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"id":"ch-1","name":"synthetic","type":0}`)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	session := newTestDiscordSession(t, server)
	p := &Platform{session: session}
	original := "untrusted @everyone <@123456789> <@&987654321>"
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "synthetic-click", AppID: "synthetic-app", Token: "synthetic-interaction", ChannelID: "ch-1", Type: discordgo.InteractionMessageComponent,
		Data:    discordgo.MessageComponentInteractionData{CustomID: "cmd:/help"},
		Message: &discordgo.Message{ID: "in-1", Content: original},
	}}
	p.handleComponentInteraction(session, i, "synthetic-user", "synthetic")
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing callback data: %#v", payload)
	}
	assertNoMentions(t, data)
	if data["content"] != original+"\n\n> /help" {
		t.Fatal("component update content changed")
	}
}
