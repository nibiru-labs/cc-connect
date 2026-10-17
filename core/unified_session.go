package core

import (
	"fmt"
	"strings"
)

// Nibiru unified_session (nibiru-labs/nibiru#966).
//
// When enabled for a project, every inbound message, whatever its platform or
// conversation, is mapped onto ONE internal session key so that all sources
// share a single agent session. The existing busy-session queue then
// serialises turns; every queued message keeps its own platform + reply ctx,
// so replies route back to the source. Lifecycle (idle reset, compress,
// rotation) is unchanged and applies to that one session.

// UnifiedSessionKey is the internal session key shared by all sources of a
// project in unified_session mode.
func UnifiedSessionKey(project string) string {
	return "unified:" + project
}

// SetUnifiedSession enables or disables unified_session for this engine.
func (e *Engine) SetUnifiedSession(v bool) {
	e.unifiedSession = v
}

// applyUnifiedSession rewrites msg.SessionKey to the unified key, remembering
// the original in OriginSessionKey. Idempotent.
func (e *Engine) applyUnifiedSession(msg *Message) {
	if !e.unifiedSession || msg == nil {
		return
	}
	key := UnifiedSessionKey(e.name)
	if msg.SessionKey == key {
		return
	}
	msg.OriginSessionKey = msg.SessionKey
	msg.SessionKey = key
}

// tagUnifiedSource prefixes msg.Content with a source header so the model
// knows who it is answering. No-op when unified_session is off or already tagged.
func (e *Engine) tagUnifiedSource(msg *Message) {
	if !e.unifiedSession || msg == nil || msg.OriginSessionKey == "" || msg.sourceTagged {
		return
	}
	msg.Content = formatUnifiedSourceTag(msg) + "\n" + msg.Content
	msg.sourceTagged = true
}

func formatUnifiedSourceTag(msg *Message) string {
	clean := strings.NewReplacer(`"`, `'`, "\n", " ", "\r", "").Replace
	var b strings.Builder
	b.WriteString("[cc-connect source")
	fmt.Fprintf(&b, " platform=%s", clean(msg.Platform))
	if msg.UserID != "" {
		fmt.Fprintf(&b, " sender_id=%s", clean(msg.UserID))
	}
	if msg.UserName != "" {
		fmt.Fprintf(&b, " sender_name=\"%s\"", clean(msg.UserName))
	}
	fmt.Fprintf(&b, " conversation=%s", clean(msg.OriginSessionKey))
	if msg.MessageID != "" {
		fmt.Fprintf(&b, " msg_id=%s", clean(msg.MessageID))
	}
	b.WriteString("]")
	return b.String()
}
