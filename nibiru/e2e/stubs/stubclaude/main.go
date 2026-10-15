// Command stubclaude stands in for the `claude` CLI in stream-json mode.
// It remembers every user turn of its own process (so "context carry-over"
// is observable), appends each received prompt to STUB_LOG, and replies
// "turn=<n> prev=<previous prompt> got=<prompt>". A prompt containing
// "[stub:fail]" produces an error result instead of a reply.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func main() {
	const sid = "stub-session-1"
	logf := func(s string) {
		p := os.Getenv("STUB_LOG")
		if p == "" {
			return
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, s)
			f.Close()
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "stub"})
	var history []string
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var in struct {
			Type    string `json:"type"`
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &in) != nil || in.Type != "user" {
			continue
		}
		prompt, _ := in.Message.Content.(string)
		logf(strings.ReplaceAll(prompt, "\n", "\\n"))
		if strings.Contains(prompt, "[stub:fail]") {
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "stub failure", "session_id": sid})
			continue
		}
		prev := "-"
		if len(history) > 0 {
			prev = history[len(history)-1]
		}
		history = append(history, prompt)
		reply := fmt.Sprintf("turn=%d prev=%s got=%s", len(history), prev, prompt)
		emit(map[string]any{"type": "assistant", "session_id": sid, "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply}},
		}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply, "session_id": sid})
	}
}
