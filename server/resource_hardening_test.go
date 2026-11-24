package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHardeningRoomBytesAndEmptyEnd(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, peer := joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < 16; i++ {
		tok := c.Token
		if i%2 == 1 {
			tok = peer
		}
		if st := sendMsg(t, srv.URL, c.RoomID, tok, strings.Repeat("x", maxMsgSize), false); st != 200 {
			t.Fatalf("fill %d: %d", i, st)
		}
	}
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "x", false); st != http.StatusServiceUnavailable {
		t.Fatalf("byte cap: %d", st)
	}
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "", true); st != 200 {
		t.Fatalf("empty end: %d", st)
	}
}

func TestHardeningRoomCountCreate(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	roomsMu.Lock()
	for i := 0; i < 1000; i++ {
		id := string(rune(i + 256))
		rooms[id] = &Room{id: id, notify: make(chan struct{}), permanent: true}
	}
	roomsMu.Unlock()
	st, body := postJSON(t, "POST", srv.URL+"/room", "", `{}`)
	if st != 503 || body["token"] != nil {
		t.Fatalf("count cap: %d %v", st, body)
	}
}

func TestHardeningEscapedAndOversizedBodies(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	// Six raw bytes per decoded control character, not a larger decoded message.
	raw := `{"text":"` + strings.Repeat(`\u0001`, maxMsgSize) + `"}`
	st, _ := postJSON(t, "POST", srv.URL+"/room/"+c.RoomID+"/msg", c.Token, raw)
	if st != 200 {
		t.Fatalf("escaped boundary: %d", st)
	}
	req := httptest.NewRequest("POST", "/room", strings.NewReader(`{"topic":"`+strings.Repeat("x", 500000)+`"}`))
	w := httptest.NewRecorder()
	http.MaxBytesHandler(http.HandlerFunc(handleCreate), 6*maxMsgSize+4096).ServeHTTP(w, req)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "request") {
		t.Fatalf("oversized: %d %s", w.Code, w.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "acq_send", "arguments": map[string]any{"uin": c.RoomID, "token": c.Token, "text": strings.Repeat("\x01", maxMsgSize)}}})
	resp, err := http.Post(srv.URL+"/mcp", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	result, _ := body["result"].(map[string]any)
	if code != 200 || body["error"] != nil || result["isError"] == true {
		t.Fatalf("MCP escaped: %d %v", code, body)
	}
}

func TestHardeningPrivateDirectory(t *testing.T) {
	old := roomsDir
	roomsDir = filepath.Join(t.TempDir(), "rooms")
	defer func() { roomsDir = old }()
	if err := storageProbe(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(roomsDir)
	if fi.Mode().Perm() != 0700 {
		t.Fatalf("mode: %o", fi.Mode().Perm())
	}
}

func TestHardeningTokenSeats(t *testing.T) {
	r := &Room{tokenA: "alpha", tokenB: "beta"}
	for _, tc := range []struct{ token, want string }{{"alpha", "A"}, {"beta", "B"}, {"", ""}, {"alph", ""}, {"alphaX", ""}, {"gamma", ""}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		if got := seatFor(r, req); got != tc.want {
			t.Fatalf("seat: %q want %q", got, tc.want)
		}
	}
}

func TestHardeningHeadersAndTimeouts(t *testing.T) {
	// Source check fixes startup configuration without waiting for real timeouts.
	b, _ := os.ReadFile("main.go")
	for _, s := range []string{"ReadHeaderTimeout: 10 * time.Second", "IdleTimeout: 120 * time.Second"} {
		if !bytes.Contains(b, []byte(s)) {
			t.Errorf("missing %s", s)
		}
	}
	srv := newTestServer(t)
	defer srv.Close()
	for _, path := range []string{"/", "/room"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Accept", "text/html")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, h := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options"} {
			if resp.Header.Get(h) == "" {
				t.Errorf("%s missing %s", path, h)
			}
		}
	}
	c := createRoom(t, srv.URL)
	resp, err := http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("credential cacheable")
	}
}

func TestHardeningBudgetFailureReloadAndReap(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	raw, _ := normalizeUIN(c.RoomID)
	r := getRoom(raw)
	budgetMu.Lock()
	before := liveBytes
	budgetMu.Unlock()
	// A definite persistence failure consumes no additional budget.
	old := persistFailMode
	persistFailMode = "pre"
	st := sendMsg(t, srv.URL, c.RoomID, c.Token, "not stored", false)
	persistFailMode = old
	if st != 500 {
		t.Fatalf("injected failure: %d", st)
	}
	budgetMu.Lock()
	after := liveBytes
	budgetMu.Unlock()
	if before != after {
		t.Fatal("failure consumed budget")
	}
	// Repeated loading replaces each room's accounting; it must not double it.
	loadRooms()
	budgetMu.Lock()
	after = liveBytes
	budgetMu.Unlock()
	if before != after {
		t.Fatal("reload double counted")
	}
	// Reserve remaining global capacity without allocating 64 MiB in a test.
	rememberSnapshot("fixture-capacity", maxLiveBytes-before)
	st, body := postJSON(t, "POST", srv.URL+"/room", "", `{}`)
	if st != 503 || body["token"] != nil {
		t.Fatalf("global create cap: %d", st)
	}
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "growth", false); st != 503 {
		t.Fatalf("global send cap: %d", st)
	}
	forgetSnapshot("fixture-capacity")
	// Use the currently mapped object after reload, not the old pointer.
	r = getRoom(raw)
	r.mu.Lock()
	r.joinDeadline = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if getRoom(raw) != nil {
		t.Fatal("expired room retained")
	}
	budgetMu.Lock()
	after = liveBytes
	budgetMu.Unlock()
	if after != 0 {
		t.Fatalf("reap retained %d budget", after)
	}
}

func TestHardeningBudgetConcurrentReservations(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	rememberSnapshot("fixture", maxLiveBytes-1500)
	var wg sync.WaitGroup
	var committed atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := roomDisk{ID: fmt.Sprintf("room%09d", i), TokenA: strings.Repeat("t", 200)}
			out, _ := writeRoom(d)
			if out != persistFailedDefinite {
				committed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	budgetMu.Lock()
	n := liveBytes
	budgetMu.Unlock()
	if n > maxLiveBytes || committed.Load() == 0 || committed.Load() == 20 {
		t.Fatalf("reservation bound: bytes=%d committed=%d", n, committed.Load())
	}
}

func TestHardeningAmbiguousWriteIsCounted(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	old := persistFailMode
	persistFailMode = "post"
	defer func() { persistFailMode = old }()
	out, _ := writeRoom(roomDisk{ID: "counted000001", TokenA: "secret"})
	budgetMu.Lock()
	n := snapshotBytes["counted000001"]
	budgetMu.Unlock()
	if out != persistAmbiguous || n == 0 {
		t.Fatal("ambiguous rename not accounted")
	}
}
