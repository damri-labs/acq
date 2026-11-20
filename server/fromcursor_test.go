package main

// Regression: every recv response carries fromCursor, the seq the read
// started from. A client that tracks the cursor it last processed can
// compare the two: a fromCursor ahead of it proves a response was lost
// or another reader sharing the seat consumed messages in between, and
// both are recoverable by re-reading with since=<last processed cursor>.
// The delivery cursor itself is unchanged: at-most-once implicit reads
// stay the default, explicit-since reads stay replay-safe.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// recvEnvelope reads like recvMsg but keeps the cursor fields.
func recvEnvelope(t *testing.T, base, uin, token, query string) (int, []Msg, int, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/room/"+uin+"/msg?wait=0"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		Messages   []Msg `json:"messages"`
		Cursor     int   `json:"cursor"`
		FromCursor int   `json:"fromCursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("recv decode: %v", err)
	}
	return res.StatusCode, body.Messages, body.Cursor, body.FromCursor
}

// Sequential implicit reads chain: each read starts where the previous
// one ended.
func TestRecvFromCursorChainsImplicitReads(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, tokenB := joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusOK {
		t.Fatalf("join: got %d", code)
	}
	sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false)
	_, msgs, cursor, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "")
	if len(msgs) != 1 || from != 0 || cursor != 1 {
		t.Fatalf("first read: msgs=%d from=%d cursor=%d, want 1/0/1", len(msgs), from, cursor)
	}
	sendMsg(t, srv.URL, c.RoomID, c.Token, "two", false)
	_, msgs, cursor, from = recvEnvelope(t, srv.URL, c.RoomID, tokenB, "")
	if len(msgs) != 1 || from != 1 || cursor != 2 {
		t.Fatalf("second read: msgs=%d from=%d cursor=%d, want 1/1/2", len(msgs), from, cursor)
	}
}

// The misuse case the field exists for: a response lost after the server
// advanced the cursor leaves the next implicit read starting ahead of
// what the client processed - visible in fromCursor, and recoverable
// with an explicit since.
func TestRecvFromCursorExposesLostResponse(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokenB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "two", false)
	_, msgs, processed, _ := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "")
	if len(msgs) != 2 || processed != 2 {
		t.Fatalf("initial read: msgs=%d cursor=%d, want 2/2", len(msgs), processed)
	}
	// A lost response: the read happens and advances the server cursor,
	// but the client discards it - its processed cursor stays behind.
	sendMsg(t, srv.URL, c.RoomID, c.Token, "three", false)
	recvEnvelope(t, srv.URL, c.RoomID, tokenB, "") // lost on the wire
	// The next implicit read starts at 3, ahead of the client's 2.
	sendMsg(t, srv.URL, c.RoomID, c.Token, "four", false)
	_, msgs, cursor, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "")
	if from != 3 || cursor != 4 {
		t.Fatalf("after loss: from=%d cursor=%d, want 3/4", from, cursor)
	}
	if from <= processed {
		t.Fatalf("gap not detectable: fromCursor %d did not run ahead of processed %d", from, processed)
	}
	if len(msgs) != 1 || msgs[0].Text != "four" {
		t.Fatalf("implicit read after loss returned %v", msgs)
	}
	// Recovery: re-reading from the last processed cursor brings the
	// lost message back.
	_, msgs, _, _ = recvEnvelope(t, srv.URL, c.RoomID, tokenB, "&since=2")
	if len(msgs) != 2 || msgs[0].Text != "three" || msgs[1].Text != "four" {
		t.Fatalf("recovery read from since=2 returned %v", msgs)
	}
}

// peek=1 advances nothing: a later implicit read still starts from the
// same delivered cursor.
func TestRecvFromCursorPeekAdvancesNothing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokenB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false)
	if _, msgs, _, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "&peek=1"); len(msgs) != 1 || from != 0 {
		t.Fatalf("peek read: msgs=%d from=%d, want 1/0", len(msgs), from)
	}
	// The peek consumed nothing, so the implicit read still starts at 0.
	if _, msgs, cursor, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, ""); len(msgs) != 1 || from != 0 || cursor != 1 {
		t.Fatalf("implicit read after peek: msgs=%d from=%d cursor=%d, want 1/0/1", len(msgs), from, cursor)
	}
}

// An explicit since is echoed as fromCursor, so a replaying client sees
// exactly where its read began.
func TestRecvFromCursorEchoesExplicitSince(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokenB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "two", false)
	if _, msgs, _, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "&since=0"); len(msgs) != 2 || from != 0 {
		t.Fatalf("replay since=0: msgs=%d from=%d, want 2/0", len(msgs), from)
	}
	if _, msgs, _, from := recvEnvelope(t, srv.URL, c.RoomID, tokenB, "&since=1"); len(msgs) != 1 || from != 1 {
		t.Fatalf("replay since=1: msgs=%d from=%d, want 1/1", len(msgs), from)
	}
}
