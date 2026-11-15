package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamPeerJoinedEvent: a seat-A stream open when seat B joins receives
// event: peer_joined, and mode=once closes the response on it.
func TestStreamPeerJoinedEvent(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/stream?mode=once", nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{"", err}
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		done <- result{string(b), err}
	}()

	// Give the stream a moment to connect, then claim seat B.
	time.Sleep(200 * time.Millisecond)
	if code, _ := joinRoom(t, srv.URL, c.RoomID); code != http.StatusOK {
		t.Fatalf("join: got %d, want 200", code)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("stream read: %v", r.err)
		}
		if !strings.Contains(r.body, "event: peer_joined") {
			t.Errorf("stream missing peer_joined event, got: %q", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close after peer join in mode=once")
	}
}

// TestStreamQuietWhenAlreadyPaired: connecting to an already-paired room
// emits no peer_joined event and no data - the transition-only rule keeps
// reconnecting watchers from hot-looping.
func TestStreamQuietWhenAlreadyPaired(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if code, _ := joinRoom(t, srv.URL, c.RoomID); code != http.StatusOK {
		t.Fatalf("join: got %d, want 200", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/room/"+c.RoomID+"/stream?mode=once", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream connect: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body) // returns on ctx timeout
	if len(b) != 0 {
		t.Errorf("stream on paired room emitted %q, want silence", string(b))
	}
}

// TestMyRoomsTopic: /my/rooms carries the room topic so it can serve as the
// signed-in user's named-room list.
func TestMyRoomsTopic(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})

	req, _ := http.NewRequest("POST", srv.URL+"/room", strings.NewReader(`{"topic":"123"}`))
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", res.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/my/rooms", nil)
	req.AddCookie(cookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("my rooms: %v", err)
	}
	defer res.Body.Close()
	var mine struct {
		Rooms []struct {
			RoomID string `json:"roomId"`
			Topic  string `json:"topic"`
		} `json:"rooms"`
	}
	json.NewDecoder(res.Body).Decode(&mine)
	if len(mine.Rooms) != 1 {
		t.Fatalf("want 1 room, got %+v", mine.Rooms)
	}
	if mine.Rooms[0].Topic != "123" {
		t.Errorf("topic = %q, want %q", mine.Rooms[0].Topic, "123")
	}

	// Signed out with auth configured: still 401.
	res, err = http.Get(srv.URL + "/my/rooms")
	if err != nil {
		t.Fatalf("my rooms anon: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon /my/rooms: got %d, want 401", res.StatusCode)
	}
}

// flushJoinWriter simulates a peer joining at the worst possible moment: the
// stream's very first Flush (headers). Deterministic - no sleeps.
type flushJoinWriter struct {
	*httptest.ResponseRecorder
	joined bool
	r      *Room
}

func (w *flushJoinWriter) Flush() {
	if !w.joined {
		w.joined = true
		w.r.mu.Lock()
		w.r.pairedAt = time.Now()
		close(w.r.notify)
		w.r.notify = make(chan struct{})
		w.r.mu.Unlock()
	}
	w.ResponseRecorder.Flush()
}

// TestStreamPeerJoinedAtHeaderFlush: regression for the snapshot-after-flush
// race - a join landing between header exposure and the pairedAt snapshot
// must still produce event: peer_joined (and close a mode=once stream).
func TestStreamPeerJoinedAtHeaderFlush(t *testing.T) {
	r := &Room{
		id:           "TESTROOM0001",
		created:      time.Now(),
		joinDeadline: time.Now().Add(time.Hour),
		notify:       make(chan struct{}),
	}
	req := httptest.NewRequest("GET", "/room/TESTROOM0001/stream?mode=once", nil)
	w := &flushJoinWriter{ResponseRecorder: httptest.NewRecorder(), r: r}

	done := make(chan struct{})
	go func() {
		handleStream(w, req, r, "A")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after join-at-flush in mode=once (snapshot race regression)")
	}
	if body := w.Body.String(); !strings.Contains(body, "event: peer_joined") {
		t.Errorf("stream missing peer_joined event, got: %q", body)
	}
}

// TestRecvPeekDoesNotAdvance: peek=1 replays without consuming delivery;
// the next cursor-free recv still sees the messages, and a second one does
// not.
func TestRecvPeekDoesNotAdvance(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusOK {
		t.Fatalf("join: got %d", code)
	}
	if code := sendMsg(t, srv.URL, c.RoomID, tokB, "one", false); code != http.StatusOK {
		t.Fatalf("send: got %d", code)
	}

	recv := func(query string) map[string]any {
		req, _ := http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		defer res.Body.Close()
		var d map[string]any
		json.NewDecoder(res.Body).Decode(&d)
		return d
	}
	count := func(d map[string]any) int { return len(d["messages"].([]any)) }

	if n := count(recv("since=0&wait=0&peek=1")); n != 1 {
		t.Fatalf("peek replay: got %d messages, want 1", n)
	}
	if n := count(recv("wait=0")); n != 1 {
		t.Fatalf("cursor-free recv after peek: got %d, want 1 (peek consumed delivery)", n)
	}
	if n := count(recv("wait=0")); n != 0 {
		t.Fatalf("second cursor-free recv: got %d, want 0", n)
	}
}
