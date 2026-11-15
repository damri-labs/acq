package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// joinRaw posts /room/{uin}/join with an arbitrary body and returns the
// status plus the decoded response.
func joinAsGuest(t *testing.T, base, uin string, body []byte) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(base+"/room/"+uin+"/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer res.Body.Close()
	var resp map[string]any
	json.NewDecoder(res.Body).Decode(&resp)
	return res.StatusCode, resp
}

// getJSON GETs a URL with an optional bearer token and decodes the body.
func getJSON(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer res.Body.Close()
	var resp map[string]any
	json.NewDecoder(res.Body).Decode(&resp)
	return res.StatusCode, resp
}

// A browser guest claims seat B with {"as":"human"}: the kind is recorded,
// echoed in the join reply, and surfaced on every read path - while messages
// from that seat still carry From "B" (the label never rewrites the wire
// seat and never changes what the seat credential can do).
func TestGuestJoinHumanKind(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	code, jr := joinAsGuest(t, srv.URL, c.RoomID, []byte(`{"as":"human"}`))
	if code != http.StatusOK {
		t.Fatalf("guest join: got %d, want 200", code)
	}
	tokB, _ := jr["token"].(string)
	if tokB == "" {
		t.Fatalf("guest join: no token in %+v", jr)
	}
	if jr["seatKind"] != "human" {
		t.Fatalf("join reply: seatKind = %v, want human", jr["seatKind"])
	}
	if jr["credential"] == nil || !strings.HasPrefix(jr["credential"].(string), "acq1_") {
		t.Fatalf("join reply: missing packed credential: %+v", jr)
	}

	// Read paths all report the self-declared kind.
	_, tr := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?view=1&wait=0", tokB)
	if tr["seatBKind"] != "human" {
		t.Fatalf("transcript view: seatBKind = %v, want human", tr["seatBKind"])
	}
	_, ra := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?wait=0", c.Token)
	if ra["seatBKind"] != "human" {
		t.Fatalf("seat A recv: seatBKind = %v, want human", ra["seatBKind"])
	}
	_, pv := getJSON(t, srv.URL+"/room/"+c.RoomID, "")
	if pv["seatBKind"] != "human" {
		t.Fatalf("preview JSON: seatBKind = %v, want human", pv["seatBKind"])
	}

	// The human guest posts as seat B; the wire From stays "B".
	if code := sendMsg(t, srv.URL, c.RoomID, tokB, "hi from a person", false); code != http.StatusOK {
		t.Fatalf("guest send: got %d, want 200", code)
	}
	code, msgs := recvMsg(t, srv.URL, c.RoomID, c.Token)
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].From != "B" {
		t.Fatalf("A recv: got code=%d msgs=%+v, want one From=B message", code, msgs)
	}

	// The kind survives a redeploy (disk roundtrip), like every other seat fact.
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()
	_, tr2 := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?view=1&wait=0", tokB)
	if tr2["seatBKind"] != "human" {
		t.Fatalf("transcript after reload: seatBKind = %v, want human", tr2["seatBKind"])
	}
}

// The join body stays optional and strictly backward compatible: no body, a
// garbage body, or an unrecognized kind all behave exactly like a classic
// curl join - and no label is recorded.
func TestGuestJoinBodyCompatible(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	for name, body := range map[string][]byte{
		"empty":   nil,
		"garbage": []byte("not json at all"),
		"unknown": []byte(`{"as":"admin"}`),
		"agent":   []byte(`{"as":"agent"}`),
	} {
		c := createRoom(t, srv.URL)
		code, jr := joinAsGuest(t, srv.URL, c.RoomID, body)
		if code != http.StatusOK {
			t.Fatalf("%s: join got %d, want 200", name, code)
		}
		if _, ok := jr["seatKind"]; ok {
			t.Fatalf("%s: unexpected seatKind in reply: %+v", name, jr)
		}
		tokB, _ := jr["token"].(string)
		_, tr := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?view=1&wait=0", tokB)
		if _, ok := tr["seatBKind"]; ok {
			t.Fatalf("%s: seatBKind recorded for non-human kind: %+v", name, tr)
		}
		// Seat semantics untouched: the seat credential works as always.
		if code := sendMsg(t, srv.URL, c.RoomID, tokB, "ping", false); code != http.StatusOK {
			t.Fatalf("%s: send as B got %d, want 200", name, code)
		}
	}
}

// The self-asserted label grants nothing: a guest-claimed B seat has exactly
// the powers of any B seat - it cannot read as A, cannot re-join, and the
// owner-probe 404 uniformity is untouched.
func TestGuestJoinKindIsNotAuthority(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, jr := joinAsGuest(t, srv.URL, c.RoomID, []byte(`{"as":"human"}`))
	tokB, _ := jr["token"].(string)

	// B cannot read A's stream (A-only cursor state is untouched).
	_, asA := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?wait=0", tokB)
	if msgs, _ := asA["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("guest B saw messages it should not: %+v", msgs)
	}
	// A second join is still a uniform 409.
	if code, _ := joinAsGuest(t, srv.URL, c.RoomID, []byte(`{"as":"human"}`)); code != http.StatusConflict {
		t.Fatalf("second guest join: got %d, want 409", code)
	}
	// An anonymous browser probe (no cookie, no token) still gets the
	// uniform 404 on /msg - the guest claim opened no new read path.
	if code, _ := getJSON(t, srv.URL+"/room/"+c.RoomID+"/msg?since=0&wait=0&view=1", ""); code != http.StatusNotFound {
		t.Fatalf("anonymous probe: got %d, want 404", code)
	}
}
