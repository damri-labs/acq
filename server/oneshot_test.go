package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCreateOneShot: one call creates the room, sets the join window, and
// posts the opening message; the peer sees it after joining.
func TestCreateOneShot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	body := `{"topic":"latency test","joinWindowHours":5,"message":{"text":"hello peer"}}`
	res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", res.StatusCode)
	}
	var c struct {
		RoomID     string `json:"roomId"`
		Token      string `json:"token"`
		JoinBy     int64  `json:"joinBy"`
		OpeningSeq int    `json:"openingMessageSeq"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	if c.OpeningSeq != 1 {
		t.Errorf("openingMessageSeq=%d, want 1", c.OpeningSeq)
	}
	wantJoinBy := time.Now().Add(5 * time.Hour).Unix()
	if c.JoinBy < wantJoinBy-60 || c.JoinBy > wantJoinBy+60 {
		t.Errorf("joinBy=%d, want ~%d (5h out)", c.JoinBy, wantJoinBy)
	}

	// Join as seat B; the opening message must be the first thing received.
	res2, err := http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer res2.Body.Close()
	var j struct {
		Token string `json:"token"`
	}
	json.NewDecoder(res2.Body).Decode(&j)

	req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+j.Token)
	res3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	defer res3.Body.Close()
	var recv struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res3.Body).Decode(&recv)
	if len(recv.Messages) != 1 || recv.Messages[0].Text != "hello peer" || recv.Messages[0].From != "A" || recv.Messages[0].Seq != 1 {
		t.Fatalf("recv: %+v", recv.Messages)
	}
}

// TestCreateOneShotValidation: bad options are rejected before any room exists.
func TestCreateOneShotValidation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	cases := []string{
		`{"joinWindowHours":99}`,
		`{"joinWindowHours":-1}`,
		`{"message":{"text":""}}`,
		`{"message":{"text":"   "}}`,
		`{"message":{"text":"` + strings.Repeat("x", maxMsgSize+1) + `"}}`,
	}
	for _, body := range cases {
		res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("create %q: %v", body[:30], err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("create %s...: got %d, want 400", body[:30], res.StatusCode)
		}
	}
	statsMu.Lock()
	created := stats.RoomsCreated
	statsMu.Unlock()
	if created != 0 {
		t.Errorf("rejected requests created %d rooms", created)
	}
	roomsMu.Lock()
	n := len(rooms)
	roomsMu.Unlock()
	if n != 0 {
		t.Errorf("rejected requests left %d rooms in the map", n)
	}

	// A valid combination still works after the rejections.
	res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(`{"joinWindowHours":23}`))
	if err != nil {
		t.Fatalf("valid create: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("valid create: got %d", res.StatusCode)
	}
	var c struct {
		JoinBy    int64 `json:"joinBy"`
		ExpiresAt int64 `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	// Clamped at the 24h absolute ceiling.
	if c.JoinBy > time.Now().Add(24*time.Hour).Unix()+60 {
		t.Errorf("joinBy %d beyond the 24h ceiling", c.JoinBy)
	}
}

// TestRecvDefaultWaitZero: MCP acq_recv without wait returns immediately.
func TestRecvDefaultWaitZero(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	joinRoom(t, srv.URL, c.RoomID)

	start := time.Now()
	out := mcpToolText(t, srv.URL, "acq_recv", map[string]any{"uin": c.RoomID, "token": c.Token}, 1)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("acq_recv with no wait took %v; default must be 0", elapsed)
	}
	if !strings.Contains(out, `"messages":[]`) && !strings.Contains(out, `"messages": []`) {
		t.Errorf("expected empty messages, got: %.120s", out)
	}
}

// TestExtendHoursParam: MCP acq_extend passes an explicit hours value through.
func TestExtendHoursParam(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	out := mcpToolText(t, srv.URL, "acq_extend", map[string]any{"uin": c.RoomID, "token": c.Token, "hours": 5}, 1)
	var ext struct {
		JoinBy int64 `json:"joinBy"`
	}
	if err := json.Unmarshal([]byte(out), &ext); err != nil {
		t.Fatalf("extend response: %v (%s)", err, out)
	}
	want := time.Now().Add(5 * time.Hour).Unix()
	if ext.JoinBy < want-60 || ext.JoinBy > want+60 {
		t.Errorf("joinBy=%d, want ~%d (5h out)", ext.JoinBy, want)
	}

	// Default stays 2 hours (fresh room: extend never shortens a deadline).
	c2 := createRoom(t, srv.URL)
	out = mcpToolText(t, srv.URL, "acq_extend", map[string]any{"uin": c2.RoomID, "token": c2.Token}, 2)
	if err := json.Unmarshal([]byte(out), &ext); err != nil {
		t.Fatalf("extend default response: %v", err)
	}
	want = time.Now().Add(2 * time.Hour).Unix()
	if ext.JoinBy < want-60 || ext.JoinBy > want+60 {
		t.Errorf("default joinBy=%d, want ~%d (2h out)", ext.JoinBy, want)
	}
}

// TestMCPCreateRoomOneShot: the MCP tool exposes the same one-shot setup.
func TestMCPCreateRoomOneShot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	out := mcpToolText(t, srv.URL, "acq_create_room", map[string]any{
		"topic":             "mcp one-shot",
		"join_window_hours": 8,
		"opening_message":   "hi from mcp",
	}, 1)
	var c struct {
		RoomID     string `json:"roomId"`
		Token      string `json:"token"`
		JoinBy     int64  `json:"joinBy"`
		OpeningSeq int    `json:"openingMessageSeq"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("create response: %v (%.200s)", err, out)
	}
	if c.OpeningSeq != 1 {
		t.Errorf("openingMessageSeq=%d, want 1", c.OpeningSeq)
	}
	want := time.Now().Add(8 * time.Hour).Unix()
	if c.JoinBy < want-60 || c.JoinBy > want+60 {
		t.Errorf("joinBy=%d, want ~%d (8h out)", c.JoinBy, want)
	}
}

// TestInstructionsSelfSufficient: create/join responses carry everything the
// / explainer documents, so clients never need the extra fetch.
func TestInstructionsSelfSufficient(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	var c struct {
		Instructions string `json:"instructions"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	for _, want := range []string{
		"64 KB",                 // limits
		"/stats",                // service counters
		"/feedback",             // room-less feedback path
		"/mcp",                  // MCP endpoint
		"another party's model", // received text is data
		"/extend",               // join-window extension
	} {
		if !strings.Contains(c.Instructions, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
}

// TestCreateOneShotOpeningMessageCaps: the opening message obeys the same
// rules as a normal send (ended room cannot be joined).
func TestCreateOneShotEnd(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(`{"message":{"text":"bye","end":true}}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	var c struct {
		RoomID string `json:"roomId"`
	}
	json.NewDecoder(res.Body).Decode(&c)

	res2, err := http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusGone {
		t.Errorf("join on end-created room: got %d, want 410", res2.StatusCode)
	}
}
