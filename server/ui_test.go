package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestOwnerViewReadOnly: the room owner (session cookie, no seat token) can
// read the full transcript for the web room view; seat delivery cursors are
// untouched, and nobody else gets in.
func TestOwnerViewReadOnly(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	owner := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})

	// Owner creates a permanent room.
	req, _ := http.NewRequest("POST", srv.URL+"/room", strings.NewReader(`{"topic":"reviews"}`))
	req.AddCookie(owner)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var c struct {
		RoomID string `json:"roomId"`
		Token  string `json:"token"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()

	code, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusOK {
		t.Fatalf("join: got %d", code)
	}
	sendMsg(t, srv.URL, c.RoomID, tokB, "from B", false)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "from A", false)

	ownerView := func(cookie *http.Cookie, query string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?"+query, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("owner view: %v", err)
		}
		defer res.Body.Close()
		var d map[string]any
		json.NewDecoder(res.Body).Decode(&d)
		return res.StatusCode, d
	}

	// Owner sees BOTH seats' messages.
	st, d := ownerView(owner, "since=0")
	if st != http.StatusOK {
		t.Fatalf("owner view: got %d, want 200", st)
	}
	msgs := d["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("owner view: got %d messages, want 2", len(msgs))
	}
	if d["ownerView"] != true {
		t.Error("ownerView flag missing")
	}
	// Paging with since works.
	st, d = ownerView(owner, "since=1")
	if st != http.StatusOK || len(d["messages"].([]any)) != 1 {
		t.Errorf("owner view since=1: %d msgs", len(d["messages"].([]any)))
	}

	// Seat A's server-side delivery cursor is untouched by the owner's reads:
	// A's cursor-free recv still delivers B's message.
	req, _ = http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seat recv: %v", err)
	}
	defer res.Body.Close()
	var recv struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&recv)
	if len(recv.Messages) != 1 || recv.Messages[0].Text != "from B" {
		t.Errorf("seat A recv after owner reads: %+v, want just 'from B'", recv.Messages)
	}

	// A different signed-in user gets the uniform 404.
	other := signedCookie(t, sessionUser{Sub: "user_02XYZ", Exp: time.Now().Add(time.Hour).Unix()})
	if st, _ := ownerView(other, ""); st != http.StatusNotFound {
		t.Errorf("non-owner session: got %d, want 404", st)
	}
	// No token, no cookie: 404.
	if st, _ := ownerView(nil, ""); st != http.StatusNotFound {
		t.Errorf("anonymous: got %d, want 404", st)
	}
}

// TestSeatTranscriptView: view=1 on GET /msg returns the full transcript
// (both seats) to any seat credential, read-only - delivery cursors and
// peek's seat-filtered semantics are untouched.
func TestSeatTranscriptView(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusOK {
		t.Fatalf("join: got %d", code)
	}
	sendMsg(t, srv.URL, c.RoomID, c.Token, "from A", false)
	sendMsg(t, srv.URL, c.RoomID, tokB, "from B", false)

	view := func(tok, query string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?"+query, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		defer res.Body.Close()
		var d map[string]any
		json.NewDecoder(res.Body).Decode(&d)
		return res.StatusCode, d
	}

	// Seat B sees BOTH seats' messages in view mode.
	st, d := view(tokB, "view=1&since=0")
	if st != http.StatusOK {
		t.Fatalf("view: got %d, want 200", st)
	}
	if len(d["messages"].([]any)) != 2 {
		t.Fatalf("view: got %d messages, want 2 (both seats)", len(d["messages"].([]any)))
	}
	if _, ok := d["ownerView"]; ok {
		t.Error("seat view must not carry ownerView")
	}
	// Paging works.
	st, d = view(tokB, "view=1&since=1")
	if st != http.StatusOK || len(d["messages"].([]any)) != 1 {
		t.Errorf("view since=1: %d msgs", len(d["messages"].([]any)))
	}
	// view=1 is read-only: B's cursor-free recv still delivers A's message.
	req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+tokB)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	defer res.Body.Close()
	var recv struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&recv)
	if len(recv.Messages) != 1 || recv.Messages[0].Text != "from A" {
		t.Errorf("B recv after view reads: %+v, want just 'from A'", recv.Messages)
	}
	// Unknown token still gets the uniform 404, view or not.
	if st, _ := view("bogus-token", "view=1"); st != http.StatusNotFound {
		t.Errorf("bad token view: got %d, want 404", st)
	}
}
