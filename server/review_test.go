package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCreateMalformedJSON: invalid bodies are rejected before any room
// exists; an empty body stays valid.
func TestCreateMalformedJSON(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	bad := []string{
		`{"joinWindowHours":"24","message":{"text":"hello"}}`, // wrong type
		`{"topic": "abc`, // truncated
		`not json at all`,
		`{"message":{"text":5}}`, // wrong nested type
	}
	for _, body := range bad {
		res, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("create %q: %v", body, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("create %q: got %d, want 400", body, res.StatusCode)
		}
	}
	statsMu.Lock()
	created := stats.RoomsCreated
	statsMu.Unlock()
	if created != 0 {
		t.Errorf("malformed requests created %d rooms", created)
	}

	// Empty body remains a valid guest create.
	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("empty create: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Errorf("empty create: got %d, want 201", res.StatusCode)
	}
}

// TestExtendPermanentSchema: permanent rooms answer extend with the same
// keys as guest rooms, zero-valued.
func TestExtendPermanentSchema(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_ext", Exp: time.Now().Add(time.Hour).Unix()})

	req, _ := http.NewRequest("POST", srv.URL+"/room", nil)
	req.AddCookie(cookie)
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

	req, _ = http.NewRequest("POST", srv.URL+"/room/"+c.RoomID+"/extend", strings.NewReader(`{"hours":2}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	defer res.Body.Close()
	var ext map[string]any
	json.NewDecoder(res.Body).Decode(&ext)
	for _, k := range []string{"joinBy", "expiresAt", "ceiling", "permanent"} {
		if _, ok := ext[k]; !ok {
			t.Errorf("extend response missing key %q: %v", k, ext)
		}
	}
}

// mcpRPCWithCookie is mcpRPC plus a session cookie.
func mcpRPCWithCookie(t *testing.T, base string, cookie *http.Cookie, tool string, args map[string]any, id int) string {
	t.Helper()
	reqBody := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}}
	b, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp %s: %v", tool, err)
	}
	defer res.Body.Close()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	json.NewDecoder(res.Body).Decode(&resp)
	if resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("mcp %s: error %+v", tool, resp.Result)
	}
	return resp.Result.Content[0].Text
}

// TestMCPAuthInterplay: auth and MCP together - a signed-in user's MCP room
// is permanent, appears in /my/rooms, and reads back as permanent over MCP.
func TestMCPAuthInterplay(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_mcp", Email: "m@x.y", Exp: time.Now().Add(time.Hour).Unix()})

	out := mcpRPCWithCookie(t, srv.URL, cookie, "acq_create_room", map[string]any{"topic": "signed mcp"}, 1)
	var c struct {
		RoomID    string `json:"roomId"`
		Permanent bool   `json:"permanent"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("mcp create response: %v (%.200s)", err, out)
	}
	if !c.Permanent || c.ExpiresAt != 0 {
		t.Errorf("mcp signed-in create: permanent=%v expiresAt=%d", c.Permanent, c.ExpiresAt)
	}

	// /my/rooms sees the MCP-created room.
	req, _ := http.NewRequest("GET", srv.URL+"/my/rooms", nil)
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("my rooms: %v", err)
	}
	defer res.Body.Close()
	var mine struct {
		Rooms []struct {
			RoomID string `json:"roomId"`
		} `json:"rooms"`
	}
	json.NewDecoder(res.Body).Decode(&mine)
	if len(mine.Rooms) != 1 || mine.Rooms[0].RoomID != c.RoomID {
		t.Fatalf("my rooms after mcp create: %+v", mine.Rooms)
	}

	// Same tool without a session still creates a guest room.
	out = mcpRPCWithCookie(t, srv.URL, nil, "acq_create_room", nil, 2)
	var g struct {
		Permanent bool  `json:"permanent"`
		ExpiresAt int64 `json:"expiresAt"`
	}
	json.Unmarshal([]byte(out), &g)
	if g.Permanent || g.ExpiresAt == 0 {
		t.Errorf("mcp guest create: permanent=%v expiresAt=%d", g.Permanent, g.ExpiresAt)
	}
}

// TestConcurrentMixedLoad: creators (signed-in and guest), /my/rooms readers,
// previewers, joiners, and senders running together. Exists to be run under
// -race; assertions only sanity-check statuses.
func TestConcurrentMixedLoad(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_race", Exp: time.Now().Add(time.Hour).Unix()})

	var wg sync.WaitGroup
	post := func(path, token string, body string, cookie *http.Cookie) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	get := func(path string, cookie *http.Cookie) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}

	// Signed-in creators who also send messages on their rooms.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				res, err := http.NewRequest("POST", srv.URL+"/room", strings.NewReader(`{"message":{"text":"hi"}}`))
				if err != nil {
					continue
				}
				// Unique source IP per creator so the per-IP create limit
				// doesn't throttle the contention this test wants.
				res.Header.Set("X-Forwarded-For", "10.9.9."+strconv.Itoa(i+1))
				res.AddCookie(cookie)
				resp, err := http.DefaultClient.Do(res)
				if err != nil {
					continue
				}
				var c struct {
					RoomID string `json:"roomId"`
					Token  string `json:"token"`
				}
				json.NewDecoder(resp.Body).Decode(&c)
				resp.Body.Close()
				post("/room/"+c.RoomID+"/msg", c.Token, `{"text":"more"}`, nil)
			}
		}(i)
	}
	// Concurrent /my/rooms readers - this is what raced before the lock fix.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				if code := get("/my/rooms", cookie); code != http.StatusOK {
					t.Errorf("my/rooms under load: %d", code)
					return
				}
			}
		}()
	}
	// Previewers and joiners on a shared room.
	shared := createRoom(t, srv.URL)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				get("/room/"+shared.RoomID, nil)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		post("/room/"+shared.RoomID+"/join", "", "", nil)
	}()
	// /new fetchers alongside everything.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				get("/new", nil)
			}
		}()
	}
	wg.Wait()
}
