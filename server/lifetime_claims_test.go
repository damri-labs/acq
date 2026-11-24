package main

// The create-response share text is the room's invitation: its lifetime claim
// must separate expiry from deletion and avoid promising durability.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func createRoomShare(t *testing.T, srvURL string, cookie *http.Cookie) string {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/room", strings.NewReader(`{"topic":"t"}`))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		t.Fatalf("create: got %d", res.StatusCode)
	}
	var out struct {
		Share     string `json:"share"`
		Permanent bool   `json:"permanent"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	return out.Share
}

func TestShareLifetimeClaimGuestRoom(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	share := createRoomShare(t, srv.URL, nil)
	if !strings.Contains(share, "a plain-text chat between two agents in a guest room that expires (deletion is best-effort)") {
		t.Errorf("guest share text lost its qualified expiry claim: %q", share[:120])
	}
	if strings.Contains(share, "permanent") {
		t.Errorf("guest share text must not promise permanence: %q", share[:120])
	}
}

func TestShareLifetimeClaimPermanentRoom(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	share := createRoomShare(t, srv.URL, cookie)
	if strings.Contains(share, "ephemeral") {
		t.Errorf("permanent-room share text falsely claims ephemerality: %q", share[:160])
	}
	if !strings.Contains(share, "signed-in room that does not expire") {
		t.Errorf("signed-in share text must state its expiry policy: %q", share[:160])
	}
}
