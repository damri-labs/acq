package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A client-supplied cid is stored on the message and echoed in both the
// send response and later self transcript reads, so a sender can identify
// THIS request after an ambiguous failure. Oversized cids are rejected
// before any commit.
func TestSendCidEcho(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	_ = tokB

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"tagged","end":false,"cid":"req-001"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent struct {
		Seq  int    `json:"seq"`
		From string `json:"from"`
		Cid  string `json:"cid"`
	}
	json.NewDecoder(res.Body).Decode(&sent)
	res.Body.Close()
	if res.StatusCode != 200 || sent.Cid != "req-001" {
		t.Fatalf("send: status=%d cid=%q", res.StatusCode, sent.Cid)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 1 || tr.Messages[0].Cid != "req-001" {
		t.Fatalf("transcript: %+v", tr.Messages)
	}

	// Messages without a cid stay cid-less; oversized cids are rejected.
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "plain", false); code != 200 {
		t.Fatalf("plain send: %d", code)
	}
	long := strings.Repeat("x", 65)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"bad","end":false,"cid":"`+long+`"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversize send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("oversized cid: want 400, got %d", res.StatusCode)
	}

	// A non-string cid fails decoding and is rejected pre-commit.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"bad2","end":false,"cid":123}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("numeric cid send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("numeric cid: want 400, got %d", res.StatusCode)
	}
}

// A cid survives the room's disk persistence: after a simulated redeploy
// (in-memory state wiped, rooms reloaded) the self transcript still
// carries it, so a verification read after a server restart stays exact.
func TestSendCidPersistence(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"durable","end":false,"cid":"req-persist"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("send: got %d", res.StatusCode)
	}

	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 1 || tr.Messages[0].Cid != "req-persist" {
		t.Fatalf("transcript after reload: %+v", tr.Messages)
	}
}
