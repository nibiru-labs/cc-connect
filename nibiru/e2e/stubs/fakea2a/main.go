// Command fakea2a stands in for the a2a CLI (`a2a_bin`). State lives in the
// JSON file named by FAKE_A2A_STATE.
//
//	fakea2a read --pending            -> JSON array of un-acked messages
//	fakea2a send --reply-to ID --key K TEXT
//	fakea2a ack ID
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/chenhg5/cc-connect/nibiru/e2e/queue"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakea2a:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	path := os.Getenv("FAKE_A2A_STATE")
	if path == "" {
		return fmt.Errorf("FAKE_A2A_STATE not set")
	}
	st := queue.Store{Path: path}
	if len(args) == 0 {
		return fmt.Errorf("no subcommand")
	}
	switch args[0] {
	case "read":
		var out []queue.Msg
		err := st.Update(func(s *queue.State) error {
			s.Reads++
			out = append(out, s.Pending...)
			return nil
		})
		if err != nil {
			return err
		}
		if out == nil {
			out = []queue.Msg{}
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "send":
		var sent queue.Sent
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--reply-to":
				i++
				sent.ReplyTo = rest[i]
			case "--key":
				i++
				sent.Key = rest[i]
			default:
				sent.Text = rest[i]
			}
		}
		return st.Update(func(s *queue.State) error {
			if s.FailSend > 0 {
				s.FailSend--
				return fmt.Errorf("simulated send failure")
			}
			for _, x := range s.Sent { // idempotent on key
				if x.Key == sent.Key {
					return nil
				}
			}
			s.Sent = append(s.Sent, sent)
			return nil
		})
	case "ack":
		if len(args) < 2 {
			return fmt.Errorf("ack: missing id")
		}
		id := args[1]
		return st.Update(func(s *queue.State) error {
			kept := s.Pending[:0]
			for _, m := range s.Pending {
				if m.MessageID != id {
					kept = append(kept, m)
				}
			}
			s.Pending = kept
			s.Acked = append(s.Acked, id)
			return nil
		})
	}
	return fmt.Errorf("unknown subcommand %q", args[0])
}
