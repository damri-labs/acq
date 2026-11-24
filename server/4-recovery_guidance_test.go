package main

// Maintained by Instinct, an AI agent acting on behalf of Tal Chaim Damri.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The verify secret and the A recovery secret are the same capability.
// This documents the existing protocol, not a newly introduced mechanism.
func TestSharedVerifyCodeCanRecoverCreatorSeat(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, b := joinRoom(t, srv.URL, c.RoomID)
	if !verifyCode(t, srv.URL, c.RoomID, b, c.PairCode) {
		t.Fatal("shared code did not verify")
	}
	status, recovered := rejoin(t, srv.URL, c.RoomID, c.PairCode, "A") // No Authorization header.
	if status != 200 {
		t.Fatalf("unauthenticated recovery status %d", status)
	}
	token, _ := recovered["token"].(string)
	if token == "" || token == c.Token {
		t.Fatal("no fresh A token")
	}
	if s := sendMsg(t, srv.URL, c.RoomID, c.Token, "old", false); s != 404 {
		t.Fatalf("retired A accepted: %d", s)
	}
	if s := sendMsg(t, srv.URL, c.RoomID, token, "new", false); s != 200 {
		t.Fatalf("new A refused: %d", s)
	}
	t.Log("Verified code also recovers A without auth; original token rejected, new token accepted")
}

func requireRecoveryWarning(t *testing.T, name, text string) {
	t.Helper()
	text = strings.Join(strings.Fields(text), " ")
	for _, s := range []string{"recover seat A without its token", "creator", "private"} {
		if !strings.Contains(text, s) {
			t.Errorf("%s missing warning %q", name, s)
		}
	}
	if !(strings.Contains(text, "Do not request or share") || strings.Contains(text, "do not share") || strings.Contains(text, "Do not share")) {
		t.Errorf("%s does not forbid verification-code sharing", name)
	}
	for _, s := range []string{"obtain the code from the creator", "spoken over a trusted channel", "Verify the pairing code your peer"} {
		if strings.Contains(text, s) {
			t.Errorf("%s still encourages sharing: %s", name, s)
		}
	}
}

func TestRecoverySecretGuidanceSurfaces(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	spec, err := os.ReadFile("../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	requireRecoveryWarning(t, "SPEC", string(spec))
	read := func(req *http.Request) string {
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 && res.StatusCode != 201 {
			t.Fatalf("status %d", res.StatusCode)
		}
		return string(b)
	}
	for _, accept := range []string{"text/plain", "text/html"} {
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Header.Set("Accept", accept)
		body := read(req)
		requireRecoveryWarning(t, "root "+accept, body)
		if accept == "text/plain" {
			a := strings.Index(body, "WHAT ACQ IS")
			z := strings.Index(body, "OPEN A ROOM")
			if a < 0 || z < a {
				t.Fatal("missing root flow boundaries")
			}
			requireRecoveryWarning(t, "root introduction", body[a:z])
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/room", nil)
	raw := read(req)
	var c map[string]any
	if e := json.Unmarshal([]byte(raw), &c); e != nil {
		t.Fatal(e)
	}
	requireRecoveryWarning(t, "create", c["instructions"].(string))
	if i := strings.Index(c["instructions"].(string), "ACQ room reference."); i < 0 {
		t.Fatal("missing reference")
	} else {
		requireRecoveryWarning(t, "create preamble", c["instructions"].(string)[:i])
	}
	uin := c["roomId"].(string)
	req, _ = http.NewRequest("GET", srv.URL+"/room/"+uin, nil)
	req.Header.Set("Accept", "application/json")
	requireRecoveryWarning(t, "room reference", read(req))
	joined := joinRaw(t, srv.URL, uin)
	requireRecoveryWarning(t, "join", joined["instructions"].(string))
	reference := joined["instructions"].(string)
	a := strings.Index(reference, "Private recovery key (pairCode):")
	z := strings.Index(reference, "Lost token recovery (rejoin):")
	if a < 0 || z < a {
		t.Error("missing adjacent verify/recovery warning")
	} else {
		requireRecoveryWarning(t, "verify paragraph", reference[a:z])
	}
	for _, tool := range mcpTools() {
		name := tool["name"].(string)
		if name == "acq_create_room" || name == "acq_verify" || name == "acq_rejoin" {
			requireRecoveryWarning(t, name, tool["description"].(string))
		}
	}
	// No verify request or secret disclosure is needed for a normal exchange.
	if s := sendMsg(t, srv.URL, uin, c["token"].(string), "private-code flow", false); s != 200 {
		t.Fatalf("A send %d", s)
	}
	if s := sendMsg(t, srv.URL, uin, joined["token"].(string), "reply", false); s != 200 {
		t.Fatalf("B send %d", s)
	}
}
