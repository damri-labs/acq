package main

// Regression: creating a room with an initial message over the 64 KB cap
// must be rejected with a readable 400 that names the cap, not a terse
// "bad message" - the same clarity the send path and the MCP transport
// already give. Validation still runs before any state exists, so a
// rejected create leaves no room behind.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// createRoomWithMessage POSTs /room with an initial message and returns
// the HTTP status and decoded error/detail body.
func createRoomWithMessage(t *testing.T, base, text string) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"message":{"text":%q}}`, text)
	resp, err := http.Post(base+"/room", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /room: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Over-cap initial message: 400 naming the 64 KB cap, and no room fields
// come back (validation runs before the room exists).
func TestCreateRoomOverCapMessageNamesCap(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	code, body := createRoomWithMessage(t, srv.URL, strings.Repeat("x", 65537))
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "65536") {
		t.Fatalf("expected cap-naming detail, got %q", detail)
	}
	if _, ok := body["roomId"]; ok {
		t.Fatalf("rejected create returned a room: %v", body)
	}
}

// Whitespace-only initial message stays a generic 400: the cap error is
// for size, not a blanket replacement for content validation.
func TestCreateRoomWhitespaceMessageStillBadMessage(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	code, body := createRoomWithMessage(t, srv.URL, "   \n\t  ")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	detail, _ := body["detail"].(string)
	if detail != "bad message" {
		t.Fatalf("expected generic bad message detail, got %q", detail)
	}
}

// An initial message at the cap boundary is accepted: the cap names a
// limit, it does not shrink it.
func TestCreateRoomMessageAtCapAccepted(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	code, body := createRoomWithMessage(t, srv.URL, strings.Repeat("x", 65536))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, body)
	}
	roomID, _ := body["roomId"].(string)
	if roomID == "" {
		t.Fatalf("create at cap missing roomId: %v", body)
	}
}
