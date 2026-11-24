package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The SDK smoke exposed a client-visible mismatch: REST permits an empty
// ending at the text budget, while MCP treated the empty string as absent.
func TestMCPEmptyEndAtRoomTextBudget(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	roomsMu.Lock()
	r := rooms[strings.ReplaceAll(c.RoomID, "-", "")]
	roomsMu.Unlock()
	r.mu.Lock()
	r.msgs = []Msg{{Seq: 1, From: "A", Text: strings.Repeat("x", maxRoomBytes+1)}}
	r.mu.Unlock()
	args := map[string]any{"uin": c.RoomID, "token": c.Token, "text": "", "end": true}
	got := mcpCall("acq_send", args, "127.0.0.1", "", "localhost", "http")
	if got["isError"] == true {
		t.Fatalf("empty ending rejected: %v", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ended || len(r.msgs) != 2 || r.msgs[1].Text != "" || !r.msgs[1].End {
		t.Fatalf("ending not stored: ended=%v messages=%d", r.ended, len(r.msgs))
	}
}

func TestMCPTextStillRequiredAndTyped(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	for _, tc := range []struct {
		name         string
		text         any
		present, end bool
	}{
		{"missing", nil, false, true}, {"null", nil, true, true},
		{"number", 12, true, true}, {"boolean", false, true, true},
		{"empty_nonending", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"uin": c.RoomID, "token": c.Token, "end": tc.end}
			if tc.present {
				args["text"] = tc.text
			}
			got := mcpCall("acq_send", args, "127.0.0.1", "", "localhost", "http")
			if got["isError"] != true {
				t.Fatalf("invalid text accepted: %v", got)
			}
			b, _ := json.Marshal(got)
			if !strings.Contains(string(b), "text") && !strings.Contains(fmt.Sprint(got), "message") {
				t.Fatalf("unclear error: %v", got)
			}
		})
	}
	if n := transcriptCount(t, srv.URL, c.RoomID, c.Token); n != 0 {
		t.Fatalf("invalid calls stored %d messages", n)
	}
}
