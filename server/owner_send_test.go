package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// createOwnerRoom creates a permanent room owned by the signed-in user and
// returns its UIN. Mirrors the setup in TestPermanentRoomLifecycle.
func createOwnerRoom(t *testing.T, srv *httptest.Server, cookie *http.Cookie) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room", nil)
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d", res.StatusCode)
	}
	var c struct {
		RoomID string `json:"roomId"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	uin, ok := normalizeUIN(c.RoomID)
	if !ok {
		t.Fatalf("bad uin %q", c.RoomID)
	}
	return uin
}

func postMsg(t *testing.T, srv *httptest.Server, uin string, cookie *http.Cookie, token string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+uin+"/msg", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		// The cookie path requires the CSRF header the web UI sends.
		req.Header.Set("X-ACQ-Owner", "1")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return res
}

// The signed-in owner posts into their own room as "H".
func TestOwnerSend(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	res := postMsg(t, srv, uin, cookie, "", `{"text":"hello from the human"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("owner post: got %d", res.StatusCode)
	}
	var ack struct {
		Seq  int    `json:"seq"`
		From string `json:"from"`
	}
	json.NewDecoder(res.Body).Decode(&ack)
	if ack.From != "H" {
		t.Errorf("owner post from=%q, want H", ack.From)
	}

	r := getRoom(uin)
	r.mu.Lock()
	if len(r.msgs) != 1 || r.msgs[0].From != "H" || r.msgs[0].Text != "hello from the human" {
		t.Fatalf("room msgs: %+v", r.msgs)
	}
	if r.ended {
		t.Error("room ended after owner post")
	}
	// Owner posts must not touch either seat's delivery cursor.
	if r.deliveredA != 0 || r.deliveredB != 0 {
		t.Errorf("cursors moved: A=%d B=%d", r.deliveredA, r.deliveredB)
	}
	r.mu.Unlock()
}

// End from an owner post is forced false: ending stays a seat action.
func TestOwnerSendCannotEnd(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	res := postMsg(t, srv, uin, cookie, "", `{"text":"not an end","end":true}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("owner post: got %d", res.StatusCode)
	}
	r := getRoom(uin)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		t.Error("owner post with end=true ended the room")
	}
	if len(r.msgs) != 1 || r.msgs[0].End {
		t.Errorf("stored msg End=%v, want false", r.msgs[0].End)
	}
}

// An H message reaches both seats' delivery queues like any peer message.
func TestOwnerSendReachesBothSeats(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	r := getRoom(uin)
	r.mu.Lock()
	tokA, tokB := r.tokenA, ""
	r.mu.Unlock()

	// Pair seat B via the join endpoint.
	res, err := http.DefaultClient.Post(srv.URL+"/room/"+uin+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	var j struct {
		Token string `json:"token"`
	}
	json.NewDecoder(res.Body).Decode(&j)
	res.Body.Close()
	tokB = j.Token
	if tokA == "" || tokB == "" {
		t.Fatalf("tokens: A=%q B=%q", tokA, tokB)
	}

	// Seat A says something, then the owner speaks.
	res = postMsg(t, srv, uin, nil, tokA, `{"text":"agent A here"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seat A post: got %d", res.StatusCode)
	}
	res = postMsg(t, srv, uin, cookie, "", `{"text":"human speaking"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("owner post: got %d", res.StatusCode)
	}

	// Seat A's queue: only the H message (its own message is excluded).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+uin+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resA, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv A: %v", err)
	}
	defer resA.Body.Close()
	var dA struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(resA.Body).Decode(&dA)
	if len(dA.Messages) != 1 || dA.Messages[0].From != "H" {
		t.Errorf("seat A queue: %+v", dA.Messages)
	}

	// Seat B's queue: A's message and the H message.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/room/"+uin+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+tokB)
	resB, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv B: %v", err)
	}
	defer resB.Body.Close()
	var dB struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(resB.Body).Decode(&dB)
	if len(dB.Messages) != 2 || dB.Messages[0].From != "A" || dB.Messages[1].From != "H" {
		t.Errorf("seat B queue: %+v", dB.Messages)
	}
}

// A stranger (no credential, or a different signed-in user) still gets the
// uniform 404 on POST - owner posting is exactly the owner's privilege.
func TestOwnerSendOnlyForOwner(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	// No cookie at all.
	res := postMsg(t, srv, uin, nil, "", `{"text":"anon"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("anon post: got %d, want 404", res.StatusCode)
	}

	// A different signed-in user.
	other := signedCookie(t, sessionUser{Sub: "user_99ZZZ", Email: "other@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	res = postMsg(t, srv, uin, other, "", `{"text":"impostor"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("other user post: got %d, want 404", res.StatusCode)
	}

	r := getRoom(uin)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.msgs) != 0 {
		t.Errorf("msgs after rejected posts: %+v", r.msgs)
	}
}

// Owner posting into an ended room gets 410, same as seats.
func TestOwnerSendEndedRoom(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	r := getRoom(uin)
	r.mu.Lock()
	tokA := r.tokenA
	r.mu.Unlock()

	res := postMsg(t, srv, uin, nil, tokA, `{"text":"bye","end":true}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seat end: got %d", res.StatusCode)
	}
	res = postMsg(t, srv, uin, cookie, "", `{"text":"too late"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Errorf("owner post to ended room: got %d, want 410", res.StatusCode)
	}
}

// The unanswered cap applies to the human voice too: H cannot spam 51 in a row.
func TestOwnerSendUnansweredCap(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	for i := 0; i < maxUnanswered; i++ {
		res := postMsg(t, srv, uin, cookie, "", `{"text":"spam"}`)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("post %d: got %d", i, res.StatusCode)
		}
	}
	res := postMsg(t, srv, uin, cookie, "", `{"text":"one too many"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("post past cap: got %d, want 429", res.StatusCode)
	}
}

// Guest rooms (no ownerID) keep the old behavior: cookie-less POST is 404.
func TestGuestRoomOwnerSendUnchanged(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.DefaultClient.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("guest create: %v", err)
	}
	var c struct {
		RoomID string `json:"roomId"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()
	uin, _ := normalizeUIN(c.RoomID)

	// Even a signed-in user who does not own the guest room gets 404.
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	res = postMsg(t, srv, uin, cookie, "", `{"text":"not my room"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("post to guest room: got %d, want 404", res.StatusCode)
	}
}

// The cookie-authenticated owner POST rejects cross-origin/simple-request
// shapes: no X-ACQ-Owner header, a cross-origin Origin, or a text/plain
// body without the header all fail with the uniform 404. Bearer seat posts
// need no header and are unaffected.
func TestOwnerSendCSRF(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	uin := createOwnerRoom(t, srv, cookie)

	do := func(name, body, contentType, ownerHeader, origin, forwardedProto string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+uin+"/msg", strings.NewReader(body))
		req.AddCookie(cookie)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if ownerHeader != "" {
			req.Header.Set("X-ACQ-Owner", ownerHeader)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}

	// Simple-request CSRF shape: text/plain body, no custom header.
	res := do("simple text/plain", `{"text":"csrf"}`, "text/plain", "", "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("no X-ACQ-Owner: got %d, want 404", res.StatusCode)
	}

	// JSON but still no header (curl without knowing the contract).
	res = do("json no header", `{"text":"csrf"}`, "application/json", "", "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("json without header: got %d, want 404", res.StatusCode)
	}

	// Cross-origin Origin is rejected even with the header.
	res = do("cross-origin", `{"text":"csrf"}`, "application/json", "1", "https://evil.example.com", "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("cross-origin Origin: got %d, want 404", res.StatusCode)
	}

	// Same-site but cross-origin (sibling subdomain) is rejected too.
	host := srv.URL[strings.Index(srv.URL, "://")+3:]
	_ = host // srv host is 127.0.0.1:port; simulate a sibling via a different host
	res = do("same-site cross-origin", `{"text":"csrf"}`, "application/json", "1", "http://other-"+host, "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("sibling-origin: got %d, want 404", res.StatusCode)
	}

	// Same-origin Origin + header passes.
	res = do("same-origin", `{"text":"legit"}`, "application/json", "1", srv.URL, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("same-origin: got %d, want 200", res.StatusCode)
	}

	// Right host but the WRONG scheme is rejected: a plain-http deployment
	// must not accept an https Origin (scheme is part of an origin).
	res = do("scheme mismatch", `{"text":"csrf"}`, "application/json", "1", "https://"+srv.URL[len("http://"):], "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("scheme mismatch: got %d, want 404", res.StatusCode)
	}

	// Behind a TLS-terminating proxy the deployment scheme comes from
	// X-Forwarded-Proto: an https Origin on the same host then matches.
	res = do("proxy https origin", `{"text":"legit"}`, "application/json", "1", "https://"+srv.URL[len("http://"):], "https")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("proxy-terminated https origin: got %d, want 200", res.StatusCode)
	}

	// Explicit deployment configuration pins the scheme regardless of the
	// connection: with ACQ_PUBLIC_SCHEME=https the http Origin fails and the
	// https Origin passes without any forwarded header.
	t.Setenv("ACQ_PUBLIC_SCHEME", "https")
	res = do("pinned scheme rejects http", `{"text":"csrf"}`, "application/json", "1", srv.URL, "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("pinned https rejects http origin: got %d, want 404", res.StatusCode)
	}
	res = do("pinned scheme accepts https", `{"text":"legit"}`, "application/json", "1", "https://"+srv.URL[len("http://"):], "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("pinned https accepts https origin: got %d, want 200", res.StatusCode)
	}
	t.Setenv("ACQ_PUBLIC_SCHEME", "")

	// Bearer seat posts need no X-ACQ-Owner and no Origin checks.
	r := getRoom(uin)
	r.mu.Lock()
	tokA := r.tokenA
	r.mu.Unlock()
	res = postMsg(t, srv, uin, nil, tokA, `{"text":"cli seat"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("bearer seat post: got %d, want 200", res.StatusCode)
	}
}
