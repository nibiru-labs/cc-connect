package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPClient implements API against the cc-connect management HTTP API
// (/api/v1/projects/...).
//
// TODO(836-core): the upstream management API does not yet expose
//   - "context_usage" {used_tokens, context_window} (or "context_percent") on
//     GET /projects/{p}/sessions items, and
//   - POST /projects/{p}/sessions/new {session_key,name,context}, which starts
//     a fresh session for the key and feeds `context` to the agent as its first
//     turn.
//
// Without them the steward sees no usage and is a no-op (safe). They need a
// small, generic core PR (candidate for upstream); this package does not
// modify core.
type HTTPClient struct {
	base  string
	token string
	hc    *http.Client
}

// NewHTTPClient builds a client for base (e.g. http://127.0.0.1:9820).
func NewHTTPClient(base, token string, hc *http.Client) *HTTPClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPClient{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return fmt.Errorf("steward: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("steward: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("steward: %s %s: status %d: %s", method, path, resp.StatusCode, truncate(string(raw), 200))
	}
	if out == nil {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("steward: decode %s: %w", path, err)
	}
	if !env.OK && env.Error != "" {
		return fmt.Errorf("steward: %s: %s", path, env.Error)
	}
	return json.Unmarshal(env.Data, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type apiSession struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SessionKey string `json:"session_key"`
	Usage      *struct {
		UsedTokens    int `json:"used_tokens"`
		ContextWindow int `json:"context_window"`
	} `json:"context_usage"`
	ContextPercent float64 `json:"context_percent"`
}

// ListSessions implements API.
func (c *HTTPClient) ListSessions(ctx context.Context, project string) ([]SessionInfo, error) {
	var d struct {
		Sessions []apiSession `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/sessions", nil, &d); err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		si := SessionInfo{Project: project, ID: s.ID, SessionKey: s.SessionKey, Name: s.Name, Percent: s.ContextPercent}
		if s.Usage != nil {
			si.UsedTokens, si.ContextWindow = s.Usage.UsedTokens, s.Usage.ContextWindow
		}
		out = append(out, si)
	}
	return out, nil
}

// History implements API.
func (c *HTTPClient) History(ctx context.Context, project, sessionID string, limit int) ([]HistoryItem, error) {
	var d struct {
		History []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"history"`
	}
	p := "/api/v1/projects/" + url.PathEscape(project) + "/sessions/" + url.PathEscape(sessionID) + "?history_limit=" + strconv.Itoa(limit)
	if err := c.do(ctx, http.MethodGet, p, nil, &d); err != nil {
		return nil, err
	}
	out := make([]HistoryItem, len(d.History))
	for i, h := range d.History {
		out[i] = HistoryItem{Role: h.Role, Content: h.Content}
	}
	return out, nil
}

// NewSession implements API. See the TODO(836-core) on HTTPClient.
func (c *HTTPClient) NewSession(ctx context.Context, project, sessionKey, name, contextText string) error {
	body := map[string]string{"session_key": sessionKey, "name": name, "context": contextText}
	return c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/sessions/new", body, nil)
}
