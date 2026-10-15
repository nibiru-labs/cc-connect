package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Inbound is one message of `a2a read --pending`.
type Inbound struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	SenderID       string `json:"sender_id"`
	InReplyTo      string `json:"in_reply_to"`
	Text           string `json:"text"`
}

// Client abstracts A2A I/O; production uses the a2a CLI, tests use a fake.
type Client interface {
	ReadPending(ctx context.Context) ([]Inbound, error)
	SendReply(ctx context.Context, replyTo, key, text string) error // send --reply-to <id> --key <key>
	Ack(ctx context.Context, messageID string) error
}

const cliTimeout = 60 * time.Second

// cliClient shells out to the a2a CLI.
type cliClient struct {
	bin string
}

func newCLIClient(bin string) *cliClient {
	if bin == "" {
		if home, err := os.UserHomeDir(); err == nil {
			bin = filepath.Join(home, "bin", "a2a")
		} else {
			bin = "a2a"
		}
	}
	return &cliClient{bin: bin}
}

func (c *cliClient) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("a2a: %s %s: %w: %s", filepath.Base(c.bin), args[0], err, strings.TrimSpace(truncate(stderr.String(), 300)))
	}
	return stdout.Bytes(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (c *cliClient) ReadPending(ctx context.Context) ([]Inbound, error) {
	out, err := c.run(ctx, "read", "--pending")
	if err != nil {
		return nil, err
	}
	return parseInbound(out)
}

func (c *cliClient) SendReply(ctx context.Context, replyTo, key, text string) error {
	_, err := c.run(ctx, "send", "--reply-to", replyTo, "--key", key, text)
	return err
}

func (c *cliClient) Ack(ctx context.Context, messageID string) error {
	_, err := c.run(ctx, "ack", messageID)
	return err
}

// parseInbound accepts a bare JSON array or an object wrapping the array
// under "messages" / "items" / "pending".
func parseInbound(data []byte) ([]Inbound, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] == '[' {
		var msgs []Inbound
		if err := json.Unmarshal(data, &msgs); err != nil {
			return nil, fmt.Errorf("a2a: parse pending: %w", err)
		}
		return msgs, nil
	}
	var wrap struct {
		Messages []Inbound `json:"messages"`
		Items    []Inbound `json:"items"`
		Pending  []Inbound `json:"pending"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, fmt.Errorf("a2a: parse pending: %w", err)
	}
	switch {
	case wrap.Messages != nil:
		return wrap.Messages, nil
	case wrap.Items != nil:
		return wrap.Items, nil
	}
	return wrap.Pending, nil
}
