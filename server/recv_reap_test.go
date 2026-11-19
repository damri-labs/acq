package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// In-flight reads after reap: handleRecv and handleStream resolve a room and
// collect their response under r.mu, then write the HTTP response after
// releasing the lock. A reap can land in that window, so a response resolved
// before the reap can be flushed after it. This file pins two classes of
// outcome:
//
//   - Merged-success requirements (always run): the content-free outcomes.
//     A fresh post-reap request is the uniform tombstone 410 regardless of
//     auth, cursor, or wait; rapid repeats fall under the documented probe
//     penalty (429, content-free); a read spanning the reap answers 410 with
//     no content.
//
//   - Diagnostic evidence (skipped unless ACQ_REAP_DIAG=1): the permissive
//     payload-crossing behavior. These document what the current build DOES
//     in the resolution/flush window. They are not a promised contract, not
//     a requirement the build must keep satisfying, and not coverage of all
//     read paths or timings; an atomic resolution/flush cutoff is not
//     asserted impossible, only unimplemented today.
//
// Gated writers make the resolution/reap ordering deterministic; the short
// real sleeps only best-effort park a long-poll and no wall-clock upper
// bound is ever asserted.

// gatedWriter blocks the first Write that carries a body byte until released,
// so a test can reap the room while the handler is mid-flush.
type gatedWriter struct {
	rr      *httptest.ResponseRecorder
	entered chan struct{} // closed when the first body Write blocks
	release chan struct{} // closed to let the blocked Write through
	once    sync.Once
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{rr: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedWriter) Header() http.Header { return g.rr.Header() }

func (g *gatedWriter) WriteHeader(code int) { g.rr.WriteHeader(code) }

func (g *gatedWriter) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.rr.Write(p)
}

func (g *gatedWriter) Flush() {}

// recvBody is the handleRecv response shape this test inspects.
type recvBody struct {
	Messages []Msg  `json:"messages"`
	Cursor   int    `json:"cursor"`
	Ended    bool   `json:"ended"`
	Detail   string `json:"detail"`
}

// TestDiagnosticRecvResolvedBeforeReapFlushesContentAfterReap records the
// observed exposure: a recv whose response was assembled before the reap
// still flushes the room's in-memory messages afterward. The handler passes
// its expired check while the room is live, collects the message, then blocks
// in Write; the sweep reaps the room (unmap + tombstone + file delete);
// releasing the gate lets the already-assembled 200 carry content that no
// longer exists anywhere else in the process. DIAGNOSTIC ONLY: skipped unless
// ACQ_REAP_DIAG=1; documents current behavior, not a contract.
func TestDiagnosticRecvResolvedBeforeReapFlushesContentAfterReap(t *testing.T) {
	if os.Getenv("ACQ_REAP_DIAG") != "1" {
		t.Skip("diagnostic: set ACQ_REAP_DIAG=1 to record the payload-crossing behavior")
	}
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	_, tokB := joinRoom(t, srv.URL, uin)
	if got := sendMsg(t, srv.URL, uin, tokB, "pre-reap content", false); got != 200 {
		t.Fatalf("send: got %d, want 200", got)
	}
	roomsMu.Lock()
	r := rooms[key]
	roomsMu.Unlock()

	gw := newGatedWriter()
	req := httptest.NewRequest("GET", "/room/"+uin+"/msg?wait=0&since=0", nil)
	done := make(chan struct{})
	go func() { handleRecv(gw, req, r, "A"); close(done) }()

	<-gw.entered // handler assembled its response and is blocked mid-flush
	r = forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)
	close(gw.release) // flush lands after the reap
	<-done

	if gw.rr.Code != 200 {
		t.Fatalf("in-flight recv answered %d, want 200 (resolved pre-reap)", gw.rr.Code)
	}
	var b recvBody
	if err := json.Unmarshal(gw.rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(b.Messages) != 1 || b.Messages[0].Text != "pre-reap content" {
		t.Fatalf("expected the pre-reap message to cross the reap, got %+v", b.Messages)
	}
}

// TestRecvAcrossReapAnswers410WithoutContent pins the content-free outcome
// for a read that spans the reap: however the read interleaves with the
// sweep - parked in its select when the sweep runs (woken by the
// reaper's notify close) or starting after it - the answer is 410
// "room expired" with no messages. Parking the poll before the reap is
// best-effort (one short sleep); no wall-clock upper bound is asserted, so
// scheduler load cannot flake the outcome.
func TestRecvAcrossReapAnswers410WithoutContent(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	_, tokB := joinRoom(t, srv.URL, uin)
	if got := sendMsg(t, srv.URL, uin, tokB, "unread at reap", false); got != 200 {
		t.Fatalf("send: got %d, want 200", got)
	}
	roomsMu.Lock()
	r := rooms[key]
	roomsMu.Unlock()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/room/"+uin+"/msg?wait=1&since=2", nil) // since past the message: nothing to deliver
	done := make(chan struct{})
	go func() { handleRecv(rr, req, r, "A"); close(done) }()

	time.Sleep(100 * time.Millisecond) // best-effort: handler parked in its select
	r = forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)
	<-done // whenever the handler completes its wait, the outcome must hold

	if rr.Code != 410 {
		t.Fatalf("blocked recv answered %d after reap, want 410", rr.Code)
	}
	var b recvBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Detail != "room expired" || len(b.Messages) != 0 {
		t.Fatalf("blocked recv body = %+v, want 410 room expired with no messages", b)
	}
}

// TestDiagnosticStreamBacklogFlushAfterReap records the same check-then-
// flush window on the SSE path: backlog collected before the reap is written
// after it. The stream then self-terminates on its next loop (expired check);
// the reaper's notify close wakes that loop promptly, with the 25s heartbeat
// as the fallback when nothing wakes a parked stream - stated here as a
// structural bound, not slept out, and not asserted as a timing guarantee.
// DIAGNOSTIC ONLY: skipped unless ACQ_REAP_DIAG=1.
func TestDiagnosticStreamBacklogFlushAfterReap(t *testing.T) {
	if os.Getenv("ACQ_REAP_DIAG") != "1" {
		t.Skip("diagnostic: set ACQ_REAP_DIAG=1 to record the payload-crossing behavior")
	}
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	_, tokB := joinRoom(t, srv.URL, uin)
	if got := sendMsg(t, srv.URL, uin, tokB, "streamed pre-reap", false); got != 200 {
		t.Fatalf("send: got %d, want 200", got)
	}
	roomsMu.Lock()
	r := rooms[key]
	roomsMu.Unlock()

	gw := newGatedWriter()
	req := httptest.NewRequest("GET", "/room/"+uin+"/stream?mode=once", nil)
	done := make(chan struct{})
	go func() { handleStream(gw, req, r, "A"); close(done) }()

	<-gw.entered // first data frame blocked mid-write
	r = forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)
	close(gw.release)
	<-done

	if !strings.Contains(gw.rr.Body.String(), "streamed pre-reap") {
		t.Fatalf("expected backlog frame to cross the reap, got %q", gw.rr.Body.String())
	}
}

// TestRecvAfterReapIs410RegardlessOfAuth pins the steady state: once reaped,
// the first fresh recv is the router's uniform tombstone 410 with the expired
// pointer - valid seat token, bogus token, explicit cursor, and wait make no
// difference, and the answer precedes seat authentication. Rapid repeats fall
// under the documented per-IP probe penalty for previously-existing rooms
// (SPEC Limits): 429 + Retry-After, also content-free.
func TestRecvAfterReapIs410RegardlessOfAuth(t *testing.T) {
	for _, auth := range []string{"valid", "bogus", "none"} {
		t.Run("auth="+auth, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.Close()
			c := createRoom(t, srv.URL)
			uin := c.RoomID
			key := strings.ReplaceAll(uin, "-", "")
			_, tokB := joinRoom(t, srv.URL, uin)
			if got := sendMsg(t, srv.URL, uin, tokB, "will be unreachable", false); got != 200 {
				t.Fatalf("send: got %d, want 200", got)
			}
			r := forceExpire(t, key)
			sweepExpired(time.Now())
			assertReaped(t, key, uin, r)

			token := "wrong"
			switch auth {
			case "valid":
				token = c.Token
			case "none":
				token = ""
			}
			// The room's own create/join/send traffic cleared no probe state for
			// tombstone lookups; answer each query shape once, 410 every time.
			for i, q := range []string{"wait=0", "wait=5&since=0", "since=1&self=1"} {
				if i > 0 {
					// Sidestep the 5s tombstone probe penalty between shapes: the
					// penalty is per-IP state keyed on the loopback test connection,
					// so reset it directly, keeping the focus on the 410 answer.
					penMu.Lock()
					penalties = map[string]*penalty{}
					penMu.Unlock()
				}
				req, _ := http.NewRequest("GET", srv.URL+"/room/"+uin+"/msg?"+q, nil)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("recv: %v", err)
				}
				var b recvBody
				json.NewDecoder(res.Body).Decode(&b)
				res.Body.Close()
				if res.StatusCode != 410 || b.Detail != "room expired" || len(b.Messages) != 0 {
					t.Fatalf("q=%q: got %d %+v, want uniform 410 room expired, no content", q, res.StatusCode, b)
				}
			}
		})
	}
}

// TestRecvRepeatAfterReapHitsProbePenalty documents the one non-410 answer a
// reaped room gives: rapid repeated lookups are the per-IP probe penalty,
// 429 + Retry-After, still carrying no room content.
func TestRecvRepeatAfterReapHitsProbePenalty(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	if _, tokB := joinRoom(t, srv.URL, uin); tokB == "" {
		t.Fatal("join failed")
	}
	r := forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)

	get := func() (int, recvBody, string) {
		req, _ := http.NewRequest("GET", srv.URL+"/room/"+uin+"/msg?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		var b recvBody
		json.NewDecoder(res.Body).Decode(&b)
		retry := res.Header.Get("Retry-After")
		res.Body.Close()
		return res.StatusCode, b, retry
	}

	if code, b, _ := get(); code != 410 || b.Detail != "room expired" {
		t.Fatalf("first post-reap recv: got %d %+v, want 410 room expired", code, b)
	}
	code, b, retry := get()
	if code != 429 || retry == "" {
		t.Fatalf("repeat post-reap recv: got %d (Retry-After %q), want 429 with Retry-After", code, retry)
	}
	if len(b.Messages) != 0 {
		t.Fatalf("429 answer carried %d messages, want none", len(b.Messages))
	}
}

// TestRecvParkedReaderWakesOnReap pins the wake-on-reap behavior: the reaper
// closes the room's notify channel, so a long-poll parked in its select
// answers 410 promptly instead of sleeping out its full wait. The park is
// best-effort: if the 100ms sleep ends before the handler reaches its select,
// the request starts after the reap and still answers 410 promptly via the
// tombstone path, so this test does not establish deterministic parking.
// wait=120 makes an unwoken handler sleep two minutes; the 10s completion
// guard is a generous event-vs-timeout distinction (12x margin), not a
// latency bound, and no universal wake latency is claimed.
func TestRecvParkedReaderWakesOnReap(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	if _, tokB := joinRoom(t, srv.URL, uin); tokB == "" {
		t.Fatal("join failed")
	}
	roomsMu.Lock()
	r := rooms[key]
	roomsMu.Unlock()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/room/"+uin+"/msg?wait=120", nil)
	done := make(chan struct{})
	go func() { handleRecv(rr, req, r, "A"); close(done) }()

	time.Sleep(100 * time.Millisecond) // best-effort: handler parked in its select
	r = forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("parked recv did not wake on reap: still blocked after 10s (wait was 120s)")
	}
	if rr.Code != 410 {
		t.Fatalf("woken recv answered %d, want 410", rr.Code)
	}
	var b recvBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Detail != "room expired" || len(b.Messages) != 0 {
		t.Fatalf("woken recv body = %+v, want 410 room expired with no messages", b)
	}
}

// TestReapPathsSerializeNoDoubleClose guards the close-without-recreate
// design: sweeping an already-reaped (unmapped) room again, or looking it up
// through the access path, must not close the notify channel a second time
// (which would panic) - reaps serialize on roomsMu and the room leaves the
// map on the first reap.
func TestReapPathsSerializeNoDoubleClose(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	r := forceExpire(t, key)
	sweepExpired(time.Now())
	assertReaped(t, key, uin, r)
	// Second sweep pass and an access-path lookup: no second reap, no panic.
	sweepExpired(time.Now())
	if got := getRoom(key); got != nil {
		t.Fatal("getRoom returned a reaped room")
	}
}
