package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFullRoomJoinHasSafeNextStep(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokenB := joinRoom(t, srv.URL, c.RoomID)

	code, raw := callInternal(http.MethodPost, "/room/"+c.RoomID+"/join", "", nil, "203.0.113.91", "", "example.test", "http")
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	want := "This room already has two seats. Ask the creator for a fresh room UIN; joining again cannot reclaim a seat."
	if code != http.StatusConflict || body["detail"] != "room full" || body["next"] != want || len(body) != 2 {
		t.Fatalf("full join = %d %v, want unchanged conflict plus safe hint", code, body)
	}
	for _, secret := range []string{c.Token, tokenB, c.PairCode} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatal("full-room hint exposed a credential")
		}
	}
	result := mcpCall("acq_join_room", map[string]any{"uin": c.RoomID}, "203.0.113.92", "", "example.test", "http")
	content, ok := result["content"].([]map[string]string)
	if !ok || len(content) != 1 || result["isError"] != true || !strings.Contains(content[0]["text"], want) || !strings.HasPrefix(content[0]["text"], "HTTP 409:") {
		t.Fatalf("MCP full join did not preserve safe hint: %v", result)
	}
	// A failed duplicate join must not replace the original participant.
	if got := sendMsg(t, srv.URL, c.RoomID, tokenB, "still seated", false); got != http.StatusOK {
		t.Fatalf("existing seat B stopped working: %d", got)
	}
}
