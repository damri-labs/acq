package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCreateMethodGate: GET /room must return 405 and must NOT mint a room
// (the source-confirmed defect this guards: handleCreate previously had no
// method check, so any GET - crawler, unfurler, browser - created a room).
// POST /room must keep working unchanged.
func TestCreateMethodGate(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	roomsCreated := func() int {
		res, err := http.Get(srv.URL + "/stats")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		defer res.Body.Close()
		var st struct {
			RoomsCreated int `json:"rooms_created"`
		}
		b, _ := io.ReadAll(res.Body)
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatalf("stats decode: %v (%q)", err, b)
		}
		return st.RoomsCreated
	}

	before := roomsCreated()

	res, err := http.Get(srv.URL + "/room")
	if err != nil {
		t.Fatalf("GET /room: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /room: got %d, want 405 (%q)", res.StatusCode, body)
	}
	if allow := res.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("GET /room: Allow header %q, want POST", allow)
	}
	if got := roomsCreated(); got != before {
		t.Fatalf("GET /room minted a room: rooms_created %d -> %d", before, got)
	}

	res2, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(`{"topic":"gate test"}`))
	if err != nil {
		t.Fatalf("POST /room: %v", err)
	}
	body2, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("POST /room: got %d, want 201 (%q)", res2.StatusCode, body2)
	}
	var cr struct {
		RoomID string `json:"roomId"`
	}
	if err := json.Unmarshal(body2, &cr); err != nil || cr.RoomID == "" {
		t.Fatalf("POST /room: no roomId in response (err=%v)", err)
	}
	if got := roomsCreated(); got != before+1 {
		t.Fatalf("POST /room: rooms_created %d -> %d, want %d", before, got, before+1)
	}
}
