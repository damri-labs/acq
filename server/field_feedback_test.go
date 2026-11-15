package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// rejoin posts a rejoin request and returns the status code and decoded body.
func rejoin(t *testing.T, base, uin, code, seat string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/room/"+uin+"/rejoin",
		strings.NewReader(`{"code":`+quote(code)+`,"seat":`+quote(seat)+`}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// joinRaw joins and returns the full decoded join response (token +
// recoveryCode + credential).
func joinRaw(t *testing.T, base, uin string) map[string]any {
	t.Helper()
	res, err := http.Post(base+"/room/"+uin+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("join: got %d, want 200", res.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return body
}

func TestRejoinRotatesToken(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	joinRoom(t, srv.URL, c.RoomID)

	// Seat A's token is lost; the operator recovers with the pairing code.
	code, body := rejoin(t, srv.URL, c.RoomID, c.PairCode, "A")
	if code != http.StatusOK {
		t.Fatalf("rejoin: got %d, want 200", code)
	}
	newTok, _ := body["token"].(string)
	if newTok == "" || newTok == c.Token {
		t.Fatalf("rejoin: token not rotated (old=%q new=%q)", c.Token, newTok)
	}
	if body["seat"] != "A" || body["credential"] == "" {
		t.Fatalf("rejoin: missing seat/credential in %v", body)
	}

	// The new token works; the old one is dead.
	if code := sendMsg(t, srv.URL, c.RoomID, newTok, "back again", false); code != http.StatusOK {
		t.Fatalf("send with rotated token: got %d, want 200", code)
	}
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "old token", false); code != http.StatusNotFound {
		t.Fatalf("send with retired token: got %d, want 404", code)
	}
}

func TestRejoinSeatB(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)

	// Seat B has not been claimed yet: recovery is a conflict, not a token.
	if code, _ := rejoin(t, srv.URL, c.RoomID, c.PairCode, "B"); code != http.StatusConflict {
		t.Fatalf("rejoin B unclaimed: got %d, want 409", code)
	}

	// Seat B proves with its own recovery code from the join response -
	// the pairing code is seat A's key and must not work for B.
	j := joinRaw(t, srv.URL, c.RoomID)
	tokB, _ := j["token"].(string)
	recB, _ := j["recoveryCode"].(string)
	if recB == "" {
		t.Fatalf("join response missing recoveryCode: %v", j)
	}
	if code, _ := rejoin(t, srv.URL, c.RoomID, c.PairCode, "B"); code != http.StatusNotFound {
		t.Fatalf("rejoin B with pairing code: got %d, want 404", code)
	}
	code, body := rejoin(t, srv.URL, c.RoomID, recB, "b")
	newTok, _ := body["token"].(string)
	if code != http.StatusOK || newTok == "" || newTok == tokB {
		t.Fatalf("rejoin B: got %d token=%q, want 200 with fresh token", code, newTok)
	}
	if code := sendMsg(t, srv.URL, c.RoomID, newTok, "B is back", false); code != http.StatusOK {
		t.Fatalf("send with recovered B token: got %d, want 200", code)
	}
}

func TestRejoinLockout(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	for i := 0; i < 5; i++ {
		if code, _ := rejoin(t, srv.URL, c.RoomID, "000000", "A"); code != http.StatusNotFound {
			t.Fatalf("wrong code attempt %d: got %d, want 404", i+1, code)
		}
	}
	// Inside the window, even the correct code is refused.
	if code, _ := rejoin(t, srv.URL, c.RoomID, c.PairCode, "A"); code != http.StatusNotFound {
		t.Fatalf("within lockout window: got %d, want 404", code)
	}
	// The rejoin counter is separate: verification is untouched.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/verify",
		strings.NewReader(`{"code":"`+c.PairCode+`"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	defer res.Body.Close()
	var v struct {
		Match bool `json:"match"`
	}
	json.NewDecoder(res.Body).Decode(&v)
	if !v.Match {
		t.Fatal("rejoin lockout spilled into the verify counter")
	}
}

// Malformed rejoin requests are rejected before touching the attempt counter.
func TestRejoinMalformedDoesNotBurnAttempts(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	for i := 0; i < 7; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/rejoin",
			strings.NewReader(`not json`))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("rejoin: %v", err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed body: got %d, want 400", res.StatusCode)
		}
	}
	// Seven malformed requests later, the real code still works on the first try.
	if code, _ := rejoin(t, srv.URL, c.RoomID, c.PairCode, "A"); code != http.StatusOK {
		t.Fatalf("after malformed flood: got %d, want 200 (counter untouched)", code)
	}
}

func TestTokenFirst(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	check := func(res *http.Response, label string) {
		t.Helper()
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		tokIdx := bytes.Index(b, []byte(`"token"`))
		instrIdx := bytes.Index(b, []byte(`"instructions"`))
		if tokIdx < 0 || instrIdx < 0 || tokIdx > instrIdx {
			t.Fatalf("%s: token must precede instructions in body", label)
		}
	}

	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	check(res, "create")

	c := createRoom(t, srv.URL)
	res, err = http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	check(res, "join")
}

func TestStatusSplit(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Each probe uses its own client IP so penalty buckets stay separate.
	joinAs := func(uin, ip string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+uin+"/join", nil)
		req.Header.Set("X-Forwarded-For", ip)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("join %s: %v", uin, err)
		}
		defer res.Body.Close()
		var d struct {
			Detail string `json:"detail"`
		}
		json.NewDecoder(res.Body).Decode(&d)
		return res.StatusCode, d.Detail
	}

	// Unknown UIN: 404, indistinguishable from a wrong token.
	if code, detail := joinAs("0000-0000-00000", "203.0.113.1"); code != http.StatusNotFound || detail != "room not found" {
		t.Fatalf("unknown UIN: got %d %q, want 404 room not found", code, detail)
	}

	// Tombstoned (expired) UIN: 410 "room expired".
	c := createRoom(t, srv.URL)
	raw := strings.ReplaceAll(c.RoomID, "-", "")
	roomsMu.Lock()
	delete(rooms, raw)
	roomsMu.Unlock()
	addTombstone(raw)
	if code, detail := joinAs(c.RoomID, "203.0.113.2"); code != http.StatusGone || detail != "room expired" {
		t.Fatalf("tombstoned UIN: got %d %q, want 410 room expired", code, detail)
	}

	// Join on a paired room: 409 "room full".
	c2 := createRoom(t, srv.URL)
	joinRoom(t, srv.URL, c2.RoomID)
	if code, detail := joinAs(c2.RoomID, "203.0.113.3"); code != http.StatusConflict || detail != "room full" {
		t.Fatalf("paired join: got %d %q, want 409 room full", code, detail)
	}

	// Wrong token on a live room: still a flat 404 (no existence oracle).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c2.RoomID+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wrong token recv: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong token: got %d, want 404", res.StatusCode)
	}
}

func TestRecvEndedRoom(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	// A ends the room with a final message.
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "last word", true); code != http.StatusOK {
		t.Fatalf("end send: got %d, want 200", code)
	}

	recvFull := func() (int, []Msg, bool) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+tokB)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Messages []Msg `json:"messages"`
			Ended    bool  `json:"ended"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		return res.StatusCode, body.Messages, body.Ended
	}

	// B drains it: 200 with the message and an ended flag.
	code, msgs, ended := recvFull()
	if code != http.StatusOK || len(msgs) != 1 || !ended {
		t.Fatalf("drain: got %d msgs=%+v ended=%v, want 200 with final message and ended=true", code, msgs, ended)
	}

	// Drained: still 200 with ended:true - the flag is the end signal, not an error.
	code, msgs, ended = recvFull()
	if code != http.StatusOK || len(msgs) != 0 || !ended {
		t.Fatalf("recv after drain: got %d msgs=%+v ended=%v, want 200 empty with ended=true", code, msgs, ended)
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var c struct {
		RoomID     string `json:"roomId"`
		Token      string `json:"token"`
		PairCode   string `json:"pairCode"`
		Credential string `json:"credential"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()

	cred, err := unpackCredential(c.Credential)
	if err != nil {
		t.Fatalf("unpack create credential: %v", err)
	}
	if cred.UIN != c.RoomID || cred.Token != c.Token || cred.PairCode != c.PairCode || cred.Seat != "A" {
		t.Fatalf("create credential round-trip: got %+v", cred)
	}

	// Join credential: seat B, recovery code inside, no pairing code.
	j := joinRaw(t, srv.URL, c.RoomID)
	jcred, err := unpackCredential(j["credential"].(string))
	if err != nil {
		t.Fatalf("unpack join credential: %v", err)
	}
	if jcred.UIN != c.RoomID || jcred.Token != j["token"] || jcred.PairCode != "" || jcred.Seat != "B" {
		t.Fatalf("join credential round-trip: got %+v", jcred)
	}
	if jcred.Recovery == "" || jcred.Recovery != j["recoveryCode"] {
		t.Fatalf("join credential missing recovery code: got %+v", jcred)
	}

	// Junk is rejected.
	if _, err := unpackCredential("nope"); err == nil {
		t.Fatal("unpack accepted a non-acq1_ string")
	}
	if _, err := unpackCredential("acq1_!!!not-base64!!!"); err == nil {
		t.Fatal("unpack accepted garbage after the prefix")
	}
}

func TestRecvSelfFlag(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "from A", false)
	sendMsg(t, srv.URL, c.RoomID, tokB, "from B", false)

	recv := func(query string) []Msg {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Messages []Msg `json:"messages"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		return body.Messages
	}

	full := recv("since=0&wait=0&self=1")
	if len(full) != 2 {
		t.Fatalf("self=1: got %d messages, want 2 (both sides)", len(full))
	}
	peerOnly := recv("since=0&wait=0")
	if len(peerOnly) != 1 || peerOnly[0].Text != "from B" {
		t.Fatalf("default: got %+v, want only B's message", peerOnly)
	}
}

// Regression: an explicit-since replay used to leave the server-side delivery
// cursor untouched, so a later implicit long-poll redelivered everything.
func TestExplicitSinceAdvancesCursor(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "seq one", false)

	// Replay with an explicit cursor...
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?since=0&wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+tokB)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var first struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&first)
	res.Body.Close()
	if len(first.Messages) != 1 {
		t.Fatalf("replay: got %+v, want the one message", first.Messages)
	}

	// ...then an implicit poll must not redeliver it.
	if _, msgs := recvMsg(t, srv.URL, c.RoomID, tokB); len(msgs) != 0 {
		t.Fatalf("implicit poll after explicit replay redelivered: %+v", msgs)
	}
}

// Regression (from review): an explicit since beyond the room's last seq must
// not poison the delivery cursor - messages that arrive after the probe are
// still delivered.
func TestReviewSinceOvershoot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?since=999&wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+tokB)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()

	sendMsg(t, srv.URL, c.RoomID, c.Token, "two", false)
	_, msgs := recvMsg(t, srv.URL, c.RoomID, tokB)
	if len(msgs) != 1 || msgs[0].Text != "two" {
		t.Fatalf("after since=999 probe, implicit poll lost new message: got %+v", msgs)
	}
}

// A token rotation via rejoin is visible to the peer seat as peerRekeyedAt.
func TestRecvPeerRekeyedAt(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	recvFlags := func(token string) map[string]any {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		defer res.Body.Close()
		var body map[string]any
		json.NewDecoder(res.Body).Decode(&body)
		return body
	}

	if _, ok := recvFlags(tokB)["peerRekeyedAt"]; ok {
		t.Fatal("peerRekeyedAt present before any rejoin")
	}
	if code, _ := rejoin(t, srv.URL, c.RoomID, c.PairCode, "A"); code != http.StatusOK {
		t.Fatalf("rejoin: got %d, want 200", code)
	}
	if _, ok := recvFlags(tokB)["peerRekeyedAt"]; !ok {
		t.Fatal("peerRekeyedAt missing for B after A rekeyed")
	}
	// A's own rekey is not reported to A as a peer event.
	if _, ok := recvFlags(c.Token)["peerRekeyedAt"]; ok {
		t.Fatal("peerRekeyedAt shown to the seat that rekeyed")
	}
}

func TestMCPRejoin(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	created := mcpToolText(t, srv.URL, "acq_create_room", map[string]any{}, 1)
	var c roomCreated
	if err := json.Unmarshal([]byte(created), &c); err != nil {
		t.Fatalf("create result not JSON: %q", created)
	}

	rejoined := mcpToolText(t, srv.URL, "acq_rejoin", map[string]any{"uin": c.RoomID, "code": c.PairCode, "seat": "A"}, 2)
	var r struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(rejoined), &r); err != nil || r.Token == "" || r.Token == c.Token {
		t.Fatalf("rejoin result: %q err=%v", rejoined, err)
	}

	// Missing arguments are a tool error, not a panic.
	out := mcpRPC(t, srv.URL, "tools/call", map[string]any{"name": "acq_rejoin", "arguments": map[string]any{"uin": c.RoomID}}, 3)
	if out["isError"] != true {
		t.Fatalf("rejoin without code/seat: got %v, want tool error", out)
	}
}

// acq_recv on a normally ended room is not a tool error: ended:true is the signal.
func TestMCPRecvEndedRoom(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	created := mcpToolText(t, srv.URL, "acq_create_room", map[string]any{}, 1)
	var c roomCreated
	if err := json.Unmarshal([]byte(created), &c); err != nil {
		t.Fatalf("create result not JSON: %q", created)
	}
	joined := mcpToolText(t, srv.URL, "acq_join_room", map[string]any{"uin": c.RoomID}, 2)
	var j struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(joined), &j)

	mcpToolText(t, srv.URL, "acq_send", map[string]any{"uin": c.RoomID, "token": c.Token, "text": "done", "end": true}, 3)

	// First recv drains the final message; second sees the drained ended room.
	for i := 0; i < 2; i++ {
		out := mcpRPC(t, srv.URL, "tools/call", map[string]any{
			"name":      "acq_recv",
			"arguments": map[string]any{"uin": c.RoomID, "token": j.Token, "wait": 0},
		}, 4+i)
		if out["isError"] == true {
			t.Fatalf("acq_recv on ended room (call %d): got tool error %v", i+1, out)
		}
		content, _ := out["content"].([]any)
		first, _ := content[0].(map[string]any)
		text, _ := first["text"].(string)
		if !strings.Contains(text, `"ended":true`) {
			t.Fatalf("acq_recv on ended room (call %d): missing ended flag: %q", i+1, text)
		}
	}
}

// codeMatch must not depend on input length (ConstantTimeCompare returns
// early on unequal lengths; the digests compared here are fixed-size).
func TestCodeMatchLengthIndependent(t *testing.T) {
	if !codeMatch("abc", "abc") {
		t.Fatal("equal codes did not match")
	}
	for _, bad := range []string{"", "ab", "abcd", "abd"} {
		if codeMatch(bad, "abc") {
			t.Fatalf("codeMatch(%q, %q) matched", bad, "abc")
		}
	}
}
