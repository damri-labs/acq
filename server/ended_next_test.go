package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The optional "next" hint on gone-room responses (SPEC.md: informational
// only). These tests pin three properties: the documented detail/status
// contract is unchanged, the hint carries the right content for the
// caller's authorization level, and uniform-404 keeps no added fields.
func TestGoneRoomNextHint(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	base := srv.URL

	c := createRoom(t, base)

	// Pair the room so join-on-ended reports ended, not full.
	res, err := http.Post(base+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var jb struct {
		Token string `json:"token"`
	}
	json.NewDecoder(res.Body).Decode(&jb)
	res.Body.Close()

	// End the room from seat A.
	if code := sendMsg(t, base, c.RoomID, c.Token, "done", true); code != http.StatusOK {
		t.Fatalf("ending send: got %d, want 200", code)
	}

	get := func(url, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("GET", base+url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	post := func(url, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", base+url, strings.NewReader(`{"text":"late"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}

	// Authenticated seat write on the ended room: contract detail unchanged,
	// hint present with the transcript-readability note.
	code, m := post("/room/"+c.RoomID+"/msg", jb.Token)
	if code != http.StatusGone || m["detail"] != "room ended" {
		t.Fatalf("send on ended: got %d %v", code, m)
	}
	next, _ := m["next"].(string)
	if !strings.Contains(next, "transcript") || !strings.Contains(next, "POST /room") {
		t.Fatalf("send on ended next: %q", next)
	}

	// Unauthenticated join on an ended-but-never-paired room (join checks
	// full before ended): hint present but WITHOUT the readability note
	// (caller may hold only a UIN).
	c3 := createRoom(t, base)
	if code := sendMsg(t, base, c3.RoomID, c3.Token, "solo close", true); code != http.StatusOK {
		t.Fatalf("ending unpaired room: got %d, want 200", code)
	}
	code, m = post("/room/"+c3.RoomID+"/join", "")
	if code != http.StatusGone || m["detail"] != "room ended" {
		t.Fatalf("join on ended: got %d %v", code, m)
	}
	next, _ = m["next"].(string)
	if next == "" || strings.Contains(next, "transcript") {
		t.Fatalf("join on ended next must omit readability note: %q", next)
	}

	// Authenticated recv on the ended room: 200, ended:true, hint present.
	code, m = get("/room/"+c.RoomID+"/msg?since=0&wait=0", jb.Token)
	if code != http.StatusOK || m["ended"] != true {
		t.Fatalf("recv on ended: got %d %v", code, m)
	}
	next, _ = m["next"].(string)
	if !strings.Contains(next, "transcript") {
		t.Fatalf("recv on ended next: %q", next)
	}

	// Tombstoned (expired) UIN: contract detail unchanged, hint present.
	raw := strings.ReplaceAll(c.RoomID, "-", "")
	roomsMu.Lock()
	delete(rooms, raw)
	roomsMu.Unlock()
	addTombstone(raw)
	code, m = get("/room/"+c.RoomID+"/msg?since=0&wait=0", jb.Token)
	if code != http.StatusGone || m["detail"] != "room expired" {
		t.Fatalf("recv on expired: got %d %v", code, m)
	}
	next, _ = m["next"].(string)
	if !strings.Contains(next, "POST /room") || strings.Contains(next, "transcript") {
		t.Fatalf("expired next: %q", next)
	}

	// Uniform-404 preservation: wrong token on a live room - exact body, no hint.
	c2 := createRoom(t, base)
	code, m = get("/room/"+c2.RoomID+"/msg?since=0&wait=0", "wrong-token")
	if code != http.StatusNotFound {
		t.Fatalf("wrong token: got %d, want 404", code)
	}
	if len(m) != 1 || m["detail"] != "room not found" {
		t.Fatalf("uniform 404 body changed: %v", m)
	}
}
