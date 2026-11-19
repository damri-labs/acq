package main

// Persistence-contract tests: candidate-first commit, definite vs ambiguous
// outcomes, notify discipline, and concurrent same-room sends. Fault
// injection uses the test-only persistFailMode hook (ACQ_TEST_PERSIST_FAIL
// is only the env seed; tests set the variable directly).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func withPersistFail(t *testing.T, mode string) {
	t.Helper()
	persistFailMode = mode
	t.Cleanup(func() { persistFailMode = "" })
}

// postJSON is a small local helper: status + decoded body.
func postJSON(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	var m map[string]any
	json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func TestSendPreRenameFailureIsDefinite(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	withPersistFail(t, "pre")
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tokB, `{"text":"lost?","cid":"c-pre"}`)
	if st != http.StatusInternalServerError || body["stored"] != false {
		t.Fatalf("pre-rename failure: got %d %v, want 500 stored=false", st, body)
	}
	// Memory untouched: the read shows nothing, the cid never landed.
	_, msgs := recvMsg(t, srv.URL, c.RoomID, c.Token)
	if len(msgs) != 0 {
		t.Fatalf("memory committed on definite failure: %+v", msgs)
	}
	// No seq/cid consumed: after repair the same send commits as seq 1.
	persistFailMode = ""
	st, body = postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tokB, `{"text":"lost?","cid":"c-pre"}`)
	if st != http.StatusOK || body["seq"] != float64(1) {
		t.Fatalf("retry after repair: got %d %v, want 200 seq=1", st, body)
	}
}

func TestSendPostRenameFailureIsAmbiguousButCommitted(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	withPersistFail(t, "post")
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tokB, `{"text":"maybe","cid":"c-post"}`)
	if st != http.StatusInternalServerError || body["stored"] != "unknown" {
		t.Fatalf("post-rename failure: got %d %v, want 500 stored=unknown", st, body)
	}
	// Committed: memory + visible file carry the message and its cid.
	_, msgs := recvMsg(t, srv.URL, c.RoomID, c.Token)
	if len(msgs) != 1 || msgs[0].Cid != "c-post" || msgs[0].Seq != 1 {
		t.Fatalf("ambiguous outcome not committed: %+v", msgs)
	}
	// cid settles it: an identical resubmission returns the original outcome.
	persistFailMode = ""
	st, dup := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tokB, `{"text":"maybe","cid":"c-post"}`)
	if st != http.StatusOK || dup["duplicate"] != true || dup["seq"] != float64(1) {
		t.Fatalf("cid resubmission: got %d %v, want 200 duplicate seq=1", st, dup)
	}
}

func TestCreatePreRenameFailureMintsNothing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	withPersistFail(t, "pre")
	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("create under pre-rename failure: got %d, want 500", res.StatusCode)
	}
	roomsMu.Lock()
	n := len(rooms)
	roomsMu.Unlock()
	if n != 0 {
		t.Fatalf("room registered on definite failure: %d rooms", n)
	}
}

func TestJoinPreRenameFailureKeepsRoomUnclaimed(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	withPersistFail(t, "pre")
	st, tok := joinRoom(t, srv.URL, c.RoomID)
	if st != http.StatusInternalServerError || tok != "" {
		t.Fatalf("join under pre-rename failure: got %d token=%q, want 500 no credential", st, tok)
	}
	// Unclaimed: a retry after repair succeeds and mints the first seat B.
	persistFailMode = ""
	st, tok = joinRoom(t, srv.URL, c.RoomID)
	if st != http.StatusOK || tok == "" {
		t.Fatalf("join retry after repair: got %d", st)
	}
}

func TestRejoinPreRenameFailureKeepsOldToken(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	uin, _ := normalizeUIN(c.RoomID)
	roomsMu.Lock()
	r := rooms[uin]
	roomsMu.Unlock()
	r.mu.Lock()
	rec := r.recoveryB
	r.mu.Unlock()

	withPersistFail(t, "pre")
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/rejoin", "", `{"seat":"B","code":"`+rec+`"}`)
	if st != http.StatusInternalServerError {
		t.Fatalf("rejoin under pre-rename failure: got %d, want 500", st)
	}
	// Old token still valid: the rotation was never committed.
	persistFailMode = ""
	st, _ = postJSON(t, http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?wait=0", tokB, "")
	if st != http.StatusOK {
		t.Fatalf("old token after definite rejoin failure: got %d, want 200", st)
	}
}

func TestExtendPreRenameFailureChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	withPersistFail(t, "pre")
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/extend", c.Token, `{"hours":4}`)
	if st != http.StatusInternalServerError {
		t.Fatalf("extend under pre-rename failure: got %d, want 500", st)
	}
	persistFailMode = ""
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/extend", c.Token, `{"hours":4}`)
	if st != http.StatusOK || body["joinBy"] == nil {
		t.Fatalf("extend retry after repair: got %d %v", st, body)
	}
}

func TestFeedbackOpenFailureIsDefinite(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	feedbackPath = t.TempDir() + "/no-such-dir/feedback.log"
	st, body := postJSON(t, http.MethodPost, srv.URL+"/feedback", "", `{"text":"hi","purpose":"test"}`)
	if st != http.StatusInternalServerError || body["stored"] != false {
		t.Fatalf("feedback open failure: got %d %v, want 500 stored=false", st, body)
	}
}

// Concurrent same-room sends from both seats: the room lock serializes
// commits, so every send succeeds exactly once with a unique contiguous seq
// and the disk file matches memory.
func TestConcurrentSameRoomSends(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	const perSeat = 10
	var wg sync.WaitGroup
	codes := make(chan int, 2*perSeat)
	for seat, tok := range map[string]string{"A": c.Token, "B": tokB} {
		for i := 0; i < perSeat; i++ {
			wg.Add(1)
			go func(tok, tag string, i int) {
				defer wg.Done()
				st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tok,
					fmt.Sprintf(`{"text":"%s-%d","cid":"%s-%d"}`, tag, i, tag, i))
				codes <- st
			}(tok, seat, i)
		}
	}
	wg.Wait()
	close(codes)
	for st := range codes {
		if st != http.StatusOK {
			t.Fatalf("concurrent send: got %d, want 200", st)
		}
	}
	uin, _ := normalizeUIN(c.RoomID)
	roomsMu.Lock()
	r := rooms[uin]
	roomsMu.Unlock()
	r.mu.Lock()
	n := len(r.msgs)
	seqs := map[int]bool{}
	for _, m := range r.msgs {
		if seqs[m.Seq] {
			t.Fatalf("duplicate seq %d", m.Seq)
		}
		seqs[m.Seq] = true
	}
	r.mu.Unlock()
	if n != 2*perSeat {
		t.Fatalf("memory holds %d messages, want %d", n, 2*perSeat)
	}
	for i := 1; i <= 2*perSeat; i++ {
		if !seqs[i] {
			t.Fatalf("seq %d missing", i)
		}
	}
}
