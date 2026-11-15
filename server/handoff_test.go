package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPreviewAgent: the share link hands a zero-context agent the room
// status, the exact join command, and the full reference - without claiming.
func TestPreviewAgent(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	res, err := http.Get(srv.URL + "/room/" + c.RoomID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview: got %d, want 200", res.StatusCode)
	}
	var p struct {
		RoomID    string `json:"roomId"`
		Status    string `json:"status"`
		Paired    bool   `json:"paired"`
		Join      string `json:"join"`
		Briefing  string `json:"briefing"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&p)
	if p.RoomID != c.RoomID || p.Paired || p.Status != "waiting for peer" {
		t.Errorf("preview: %+v", p)
	}
	if !strings.HasSuffix(p.Join, "/room/"+c.RoomID+"/join") || !strings.HasPrefix(p.Join, "curl -s -X POST ") {
		t.Errorf("join command: %q", p.Join)
	}
	for _, want := range []string{"seat token", "/msg", "stranger"} {
		if !strings.Contains(p.Briefing, want) {
			t.Errorf("briefing missing %q", want)
		}
	}

	// Previewing twice claims nothing: a join still succeeds.
	res2, err := http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join after previews: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("join after previews: got %d, want 200", res2.StatusCode)
	}

	// After joining, the preview reports the room as paired.
	res3, err := http.Get(srv.URL + "/room/" + c.RoomID)
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	defer res3.Body.Close()
	var p2 struct {
		Paired bool `json:"paired"`
	}
	json.NewDecoder(res3.Body).Decode(&p2)
	if !p2.Paired {
		t.Error("preview after join: paired=false")
	}
}

// TestPreviewBrowser: browsers get the static room page (invite view / live transcript).
func TestPreviewBrowser(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID, nil)
	req.Header.Set("Accept", "text/html")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preview html: %v", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type: %q", ct)
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	page := b.String()
	// The room page is fully static: the JS derives the UIN from the URL,
	// so the served HTML carries the shell, not the room id.
	if !strings.Contains(page, "ACQ room") || !strings.Contains(page, "/join") || !strings.Contains(page, "location.pathname") {
		t.Errorf("html preview missing room page shell or join command")
	}
}

// TestPreviewUnknown: unknown UINs get the uniform 404 (no existence leak).
func TestPreviewUnknown(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/room/K4TZ-9M0X-3PW7Q")
	if err != nil {
		t.Fatalf("preview unknown: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("preview unknown: got %d, want 404", res.StatusCode)
	}
	var body struct {
		Detail string `json:"detail"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	if body.Detail != "room not found" {
		t.Errorf("detail: %q", body.Detail)
	}
}

// TestNewEndpoint: one fetch = one fresh provisional room; two fetches = two
// rooms (race-free by construction).
func TestNewEndpoint(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	get := func() struct {
		RoomID      string `json:"roomId"`
		Token       string `json:"token"`
		Provisional bool   `json:"provisional"`
		JoinBy      int64  `json:"joinBy"`
	} {
		res, err := http.Get(srv.URL + "/new")
		if err != nil {
			t.Fatalf("GET /new: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("GET /new: got %d, want 201", res.StatusCode)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control: %q", cc)
		}
		var out struct {
			RoomID      string `json:"roomId"`
			Token       string `json:"token"`
			Provisional bool   `json:"provisional"`
			JoinBy      int64  `json:"joinBy"`
		}
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}

	a := get()
	if !a.Provisional {
		t.Error("/new room not marked provisional")
	}
	want := time.Now().Add(30 * time.Minute).Unix()
	if a.JoinBy < want-60 || a.JoinBy > want+60 {
		t.Errorf("joinBy=%d, want ~%d (30m out)", a.JoinBy, want)
	}
	b := get()
	if a.RoomID == b.RoomID {
		t.Error("two /new fetches returned the same room")
	}

	// A provisional room joins normally.
	res, err := http.Post(srv.URL+"/room/"+a.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join provisional: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("join provisional: got %d, want 200", res.StatusCode)
	}
}

// TestProvisionalFlag: the REST flag works, joinWindowHours wins when both
// are set, and the flag survives a persistence reload.
func TestProvisionalFlag(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(`{"provisional":true}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	var c struct {
		RoomID      string `json:"roomId"`
		Provisional bool   `json:"provisional"`
		JoinBy      int64  `json:"joinBy"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	if !c.Provisional {
		t.Error("provisional flag not set")
	}
	want := time.Now().Add(30 * time.Minute).Unix()
	if c.JoinBy < want-60 || c.JoinBy > want+60 {
		t.Errorf("joinBy=%d, want ~%d (30m out)", c.JoinBy, want)
	}

	// Persistence round trip keeps the flag and the short deadline.
	uin, _ := normalizeUIN(c.RoomID)
	roomsMu.Lock()
	r := rooms[uin]
	roomsMu.Unlock()
	saveRoom(r)
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()
	roomsMu.Lock()
	r2 := rooms[uin]
	roomsMu.Unlock()
	if r2 == nil || !r2.provisional {
		t.Fatal("provisional room lost flag after reload")
	}
	if r2.joinDeadline.Sub(r2.created) > 31*time.Minute {
		t.Errorf("reloaded deadline: %v after creation", r2.joinDeadline.Sub(r2.created))
	}

	// joinWindowHours wins over provisional.
	res2, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(`{"provisional":true,"joinWindowHours":5}`))
	if err != nil {
		t.Fatalf("create both: %v", err)
	}
	defer res2.Body.Close()
	var c2 struct {
		Provisional bool  `json:"provisional"`
		JoinBy      int64 `json:"joinBy"`
	}
	json.NewDecoder(res2.Body).Decode(&c2)
	want = time.Now().Add(5 * time.Hour).Unix()
	if c2.JoinBy < want-60 || c2.JoinBy > want+60 {
		t.Errorf("joinBy=%d, want ~%d (5h: joinWindowHours should win)", c2.JoinBy, want)
	}
}

// TestShareLineHasInviteLink: the create response's share text leads with the
// preview URL so forwarding it works with zero context.
func TestShareLineHasInviteLink(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	var c struct {
		RoomID string `json:"roomId"`
		Share  string `json:"share"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	if !strings.Contains(c.Share, "/room/"+c.RoomID) {
		t.Errorf("share line missing invite link: %s", c.Share)
	}
	if !strings.Contains(c.Share, "invite link") {
		t.Errorf("share line does not present the link as the handoff: %s", c.Share)
	}
}
