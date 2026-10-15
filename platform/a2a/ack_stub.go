package a2a

import "context"

// TODO(836-a2a-core): minimal stubs of the shared contract (spec/a2a/836-cc-connect-unified-dispatcher.md).
// L1 owns the real definitions in a2a.go / client.go; delete this file when merging with L1.

// Inbound is one message from `a2a read --pending`.
type Inbound struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	SenderID       string `json:"sender_id"`
	InReplyTo      string `json:"in_reply_to"`
	Text           string `json:"text"`
}

// Client abstracts A2A send/receive.
type Client interface {
	ReadPending(ctx context.Context) ([]Inbound, error)
	SendReply(ctx context.Context, replyTo, key, text string) error
	Ack(ctx context.Context, messageID string) error
}

// Acker is called by the platform around each engine turn.
type Acker interface {
	Track(sessionKey string, messageIDs []string)
	TurnSucceeded(sessionKey string)
	TurnFailed(sessionKey string, err error)
}
