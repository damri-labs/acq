package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A client-supplied cid is stored on the message and echoed in both the
// send response and later self transcript reads, so a sender can identify
// THIS request after an ambiguous failure. Oversized cids are rejected
// before any commit.
func TestSendCidEcho(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	_ = tokB

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"tagged","end":false,"cid":"req-001"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent struct {
		Seq  int    `json:"seq"`
		From string `json:"from"`
		Cid  string `json:"cid"`
	}
	json.NewDecoder(res.Body).Decode(&sent)
	res.Body.Close()
	if res.StatusCode != 200 || sent.Cid != "req-001" {
		t.Fatalf("send: status=%d cid=%q", res.StatusCode, sent.Cid)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 1 || tr.Messages[0].Cid != "req-001" {
		t.Fatalf("transcript: %+v", tr.Messages)
	}

	// Messages without a cid stay cid-less; oversized cids are rejected.
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "plain", false); code != 200 {
		t.Fatalf("plain send: %d", code)
	}
	long := strings.Repeat("x", 65)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"bad","end":false,"cid":"`+long+`"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversize send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("oversized cid: want 400, got %d", res.StatusCode)
	}

	// A non-string cid fails decoding and is rejected pre-commit.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"bad2","end":false,"cid":123}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("numeric cid send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("numeric cid: want 400, got %d", res.StatusCode)
	}
}

// A cid survives the room's disk persistence: after a simulated redeploy
// (in-memory state wiped, rooms reloaded) the self transcript still
// carries it, so a verification read after a server restart stays exact.
func TestSendCidPersistence(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(`{"text":"durable","end":false,"cid":"req-persist"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("send: got %d", res.StatusCode)
	}

	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 1 || tr.Messages[0].Cid != "req-persist" {
		t.Fatalf("transcript after reload: %+v", tr.Messages)
	}
}

// A resubmitted send (same seat, same cid, same text) - a browser's
// transparent retry of a dropped committed response, or an explicit client
// retry - returns the original outcome without committing a duplicate.
// Reusing the cid with different text is refused as a binding mismatch.
// The same cid from the other seat is an independent logical send.
func TestSendCidResubmission(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	post := func(tok, body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		var m map[string]any
		json.NewDecoder(res.Body).Decode(&m)
		res.Body.Close()
		return res.StatusCode, m
	}

	code, first := post(c.Token, `{"text":"once","end":false,"cid":"retry-1"}`)
	if code != 200 {
		t.Fatalf("first send: %d %v", code, first)
	}
	code, dup := post(c.Token, `{"text":"once","end":false,"cid":"retry-1"}`)
	if code != 200 || dup["duplicate"] != true || dup["seq"] != first["seq"] {
		t.Fatalf("resubmission: %d %v (first %v)", code, dup, first)
	}

	// Binding mismatch: same cid, different payload - explicit refusal, no
	// commit. The end flag is part of the bound payload too.
	code, mm := post(c.Token, `{"text":"twice","end":false,"cid":"retry-1"}`)
	if code != http.StatusConflict {
		t.Fatalf("mismatched resubmission: %d %v", code, mm)
	}
	code, mm = post(c.Token, `{"text":"once","end":true,"cid":"retry-1"}`)
	if code != http.StatusConflict {
		t.Fatalf("end-flag mismatch: %d %v", code, mm)
	}

	// The peer's use of the same cid string is independent.
	code, peer := post(tokB, `{"text":"peer send","end":false,"cid":"retry-1"}`)
	if code != 200 || peer["duplicate"] == true {
		t.Fatalf("peer send: %d %v", code, peer)
	}

	// Exactly two commits: the original and the peer's.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, _ := http.DefaultClient.Do(req)
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "once" || tr.Messages[1].Text != "peer send" {
		t.Fatalf("transcript: %+v", tr.Messages)
	}
	// Reverse direction: an end send bound first, retried without the flag.
	code, mm = post(c.Token, `{"text":"","end":true,"cid":"end-1"}`)
	if code != http.StatusOK {
		t.Fatalf("end send: %d %v", code, mm)
	}
	code, mm = post(c.Token, `{"text":"","end":false,"cid":"end-1"}`)
	if code != http.StatusConflict {
		t.Fatalf("reverse end-flag mismatch: %d %v", code, mm)
	}
}

// Ordering guarantees of cid resubmission: concurrent same-cid retries
// commit exactly once; the id's memory survives a process restart (it is
// stored on the persisted message); a resubmission after the room ended
// still returns the original outcome, while a fresh cid on an ended room
// is refused as ended.
func TestSendCidResubmissionOrdering(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	post := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		var m map[string]any
		json.NewDecoder(res.Body).Decode(&m)
		res.Body.Close()
		return res.StatusCode, m
	}

	// Concurrent retries of one logical send: exactly one commit.
	var wg sync.WaitGroup
	codes := make([]int, 8)
	dups := make([]bool, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, resp := post(`{"text":"raced","end":false,"cid":"race-1"}`)
			codes[i] = code
			dups[i] = resp["duplicate"] == true
		}(i)
	}
	wg.Wait()
	deduped := 0
	for i := range codes {
		if codes[i] != 200 {
			t.Fatalf("concurrent resubmission: codes %v", codes)
		}
		if dups[i] {
			deduped++
		}
	}
	if deduped != len(codes)-1 {
		t.Fatalf("expected exactly one original commit, duplicates=%d of %d", deduped, len(codes))
	}

	// Simulated restart: in-memory rooms dropped, reloaded from disk.
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()
	code, resp := post(`{"text":"raced","end":false,"cid":"race-1"}`)
	if code != 200 || resp["duplicate"] != true {
		t.Fatalf("resubmission after restart: %d %v", code, resp)
	}

	// End the room, then resubmit the original cid: the original outcome
	// is still returned (it did commit); a fresh cid meets the ended room.
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "", true); code != 200 {
		t.Fatalf("end send: %d", code)
	}
	code, resp = post(`{"text":"raced","end":false,"cid":"race-1"}`)
	if code != 200 || resp["duplicate"] != true {
		t.Fatalf("resubmission after end: %d %v", code, resp)
	}
	code, _ = post(`{"text":"new","end":false,"cid":"fresh-1"}`)
	if code != http.StatusGone {
		t.Fatalf("fresh cid on ended room: %d", code)
	}

	// Exactly two commits across the whole test: raced + the end marker.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?self=1&peek=1&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, _ := http.DefaultClient.Do(req)
	var tr struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "raced" || !tr.Messages[1].End {
		t.Fatalf("transcript: %+v", tr.Messages)
	}
}
