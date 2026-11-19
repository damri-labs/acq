package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// setAuthEnv points auth at a fake WorkOS API and returns the config the
// handlers will see.
func setAuthEnv(t *testing.T, apiBase string) {
	t.Helper()
	t.Setenv("WORKOS_CLIENT_ID", "client_test123")
	t.Setenv("WORKOS_API_KEY", "sk_test_456")
	t.Setenv("ACQ_SESSION_SECRET", "test-session-secret")
	if apiBase != "" {
		t.Setenv("WORKOS_API_BASE", apiBase)
	}
}

func signedCookie(t *testing.T, u sessionUser) *http.Cookie {
	t.Helper()
	return &http.Cookie{Name: sessionCookie, Value: signSession(u, authCfg().sessionKey)}
}

func TestSessionSignVerify(t *testing.T) {
	setAuthEnv(t, "")
	key := authCfg().sessionKey
	u := sessionUser{Sub: "user_01", Email: "a@b.c", Name: "A B", Exp: time.Now().Add(time.Hour).Unix()}
	v := signSession(u, key)
	got := verifySession(v, key)
	if got == nil || got.Sub != "user_01" || got.Email != "a@b.c" || got.Name != "A B" {
		t.Fatalf("round trip failed: %+v", got)
	}
	// Tampered payload must fail.
	parts := strings.Split(v, ".")
	tampered := parts[0][:len(parts[0])-2] + "XX." + parts[1]
	if verifySession(tampered, key) != nil {
		t.Fatal("tampered session accepted")
	}
	// Wrong key must fail.
	other := sha256.Sum256([]byte("acq-session-v1\x00" + "other-secret"))
	if verifySession(v, other[:]) != nil {
		t.Fatal("session verified under wrong key")
	}
	// Expired session must fail.
	old := sessionUser{Sub: "user_01", Exp: time.Now().Add(-time.Hour).Unix()}
	if verifySession(signSession(old, key), key) != nil {
		t.Fatal("expired session accepted")
	}
	// Garbage must fail.
	if verifySession("not-a-session", key) != nil {
		t.Fatal("garbage session accepted")
	}
}

func TestAuthLoginRedirect(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	client := noRedirectClient()
	res, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("login: got %d, want 302", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	for _, want := range []string{
		"https://api.workos.com/user_management/authorize?",
		"response_type=code",
		"client_id=client_test123",
		"provider=authkit",
		"redirect_uri=" + url.QueryEscape(strings.Replace(srv.URL, "http", "https", 1)+"/auth/callback"),
		"state=",
	} {
		if !strings.Contains(loc, want) {
			t.Errorf("Location missing %q: %s", want, loc)
		}
	}
	var stateCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatal("no oauth state cookie set")
	}
	if !strings.Contains(loc, "state="+stateCookie.Value) {
		t.Error("state param does not match state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie not HttpOnly")
	}
}

func TestAuthDisabled(t *testing.T) {
	// No WORKOS_* env: auth endpoints 404 and rooms stay guest rooms.
	srv := newTestServer(t)
	defer srv.Close()
	for _, p := range []string{"/auth/login", "/auth/callback", "/auth/me", "/auth/logout", "/my/rooms"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", p, res.StatusCode)
		}
	}
}

// fakeWorkOS serves /user_management/authenticate for callback tests.
func fakeWorkOS(t *testing.T, failCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user_management/authenticate" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != "client_test123" || body["client_secret"] != "sk_test_456" || body["grant_type"] != "authorization_code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if failCode != 0 || body["code"] != "good-code" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "invalid_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"user": map[string]any{
				"id": "user_01ABC", "email": "tal@example.com",
				"first_name": "Tal", "last_name": "Damri",
			},
			"access_token": "at_xxx",
		})
	}))
}

// loginFlow performs /auth/login and returns the state cookie and state value.
func loginFlow(t *testing.T, client *http.Client, base string) (*http.Cookie, string) {
	t.Helper()
	res, err := client.Get(base + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	res.Body.Close()
	loc := res.Header.Get("Location")
	var sc *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == oauthStateCookie {
			sc = c
		}
	}
	if sc == nil {
		t.Fatal("no state cookie from login")
	}
	const marker = "state="
	i := strings.Index(loc, marker)
	if i < 0 {
		t.Fatal("no state in redirect")
	}
	state := loc[i+len(marker):]
	if j := strings.IndexByte(state, '&'); j >= 0 {
		state = state[:j]
	}
	return sc, state
}

func TestAuthCallback(t *testing.T) {
	fake := fakeWorkOS(t, 0)
	defer fake.Close()
	setAuthEnv(t, fake.URL)
	srv := newTestServer(t)
	defer srv.Close()
	client := noRedirectClient()

	_, state := loginFlow(t, client, srv.URL)

	// Wrong state is rejected before any code exchange.
	req, _ := http.NewRequest("GET", srv.URL+"/auth/callback?code=good-code&state=wrong", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: state})
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback bad state: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: got %d, want 400", res.StatusCode)
	}

	// Bad code is rejected by the provider.
	req, _ = http.NewRequest("GET", srv.URL+"/auth/callback?code=bad-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: state})
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("callback bad code: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad code: got %d, want 401", res.StatusCode)
	}

	// Happy path: session cookie set, redirected home.
	req, _ = http.NewRequest("GET", srv.URL+"/auth/callback?code=good-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: state})
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Fatalf("callback: got %d loc=%q, want 302 /", res.StatusCode, res.Header.Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil || sess.Value == "" {
		t.Fatal("no session cookie set")
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie flags: HttpOnly=%v SameSite=%v", sess.HttpOnly, sess.SameSite)
	}

	// /auth/me now reports the user.
	req, _ = http.NewRequest("GET", srv.URL+"/auth/me", nil)
	req.AddCookie(sess)
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer res.Body.Close()
	var me struct {
		User *sessionUser `json:"user"`
	}
	json.NewDecoder(res.Body).Decode(&me)
	if me.User == nil || me.User.Sub != "user_01ABC" || me.User.Email != "tal@example.com" || me.User.Name != "Tal Damri" {
		t.Fatalf("me: %+v", me.User)
	}

	// Logout clears the cookie.
	req, _ = http.NewRequest("GET", srv.URL+"/auth/logout", nil)
	req.AddCookie(sess)
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	res.Body.Close()
	var cleared bool
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear session cookie")
	}
}

func TestAuthMeGuest(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer res.Body.Close()
	var me struct {
		User *sessionUser `json:"user"`
	}
	json.NewDecoder(res.Body).Decode(&me)
	if me.User != nil {
		t.Fatalf("guest me: %+v", me.User)
	}
}

func TestPermanentRoomLifecycle(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	cookie := signedCookie(t, sessionUser{Sub: "user_01ABC", Email: "tal@example.com", Exp: time.Now().Add(time.Hour).Unix()})

	// Create a room while signed in.
	req, _ := http.NewRequest("POST", srv.URL+"/room", nil)
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
		RoomID    string `json:"roomId"`
		Token     string `json:"token"`
		Permanent bool   `json:"permanent"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	if !c.Permanent {
		t.Error("signed-in create: permanent not set")
	}
	if c.ExpiresAt != 0 {
		t.Errorf("signed-in create: expiresAt=%d, want 0", c.ExpiresAt)
	}

	// The room must not expire even far past the absolute TTL.
	uin, ok := normalizeUIN(c.RoomID)
	if !ok {
		t.Fatalf("bad uin %q", c.RoomID)
	}
	roomsMu.Lock()
	r := rooms[uin]
	roomsMu.Unlock()
	if r == nil {
		t.Fatal("room not in map")
	}
	if !r.permanent || r.ownerID != "user_01ABC" {
		t.Errorf("room fields: permanent=%v owner=%q", r.permanent, r.ownerID)
	}
	if r.expired(time.Now().Add(365 * 24 * time.Hour)) {
		t.Error("permanent room reports expired a year out")
	}

	// Persistence round trip: a permanent room survives save+load.
	r.mu.Lock()
	if outcome, err := writeRoom(roomDiskFrom(r)); outcome != persistOK {
		r.mu.Unlock()
		t.Fatalf("writeRoom: outcome=%v err=%v", outcome, err)
	}
	r.mu.Unlock()
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()
	roomsMu.Lock()
	r2 := rooms[uin]
	roomsMu.Unlock()
	if r2 == nil {
		t.Fatal("permanent room lost after reload")
	}
	if !r2.permanent || r2.ownerID != "user_01ABC" {
		t.Errorf("reloaded room: permanent=%v owner=%q", r2.permanent, r2.ownerID)
	}

	// Join it: expiresAt stays 0 and permanent is reported.
	res, err = http.Post(srv.URL+"/room/"+c.RoomID+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer res.Body.Close()
	var j struct {
		Token     string `json:"token"`
		Permanent bool   `json:"permanent"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&j)
	if !j.Permanent || j.ExpiresAt != 0 {
		t.Errorf("join: permanent=%v expiresAt=%d", j.Permanent, j.ExpiresAt)
	}

	// Recv reports permanent and expiresAt 0.
	req, _ = http.NewRequest("GET", srv.URL+"/room/"+c.RoomID+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+j.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	defer res.Body.Close()
	var recv struct {
		Permanent bool  `json:"permanent"`
		ExpiresAt int64 `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&recv)
	if !recv.Permanent || recv.ExpiresAt != 0 {
		t.Errorf("recv: permanent=%v expiresAt=%d", recv.Permanent, recv.ExpiresAt)
	}

	// Extend is a harmless no-op on permanent rooms.
	req, _ = http.NewRequest("POST", srv.URL+"/room/"+c.RoomID+"/extend", strings.NewReader(`{"hours":2}`))
	req.Header.Set("Authorization", "Bearer "+j.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	defer res.Body.Close()
	var ext struct {
		Permanent bool  `json:"permanent"`
		ExpiresAt int64 `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&ext)
	if !ext.Permanent || ext.ExpiresAt != 0 {
		t.Errorf("extend: permanent=%v expiresAt=%d", ext.Permanent, ext.ExpiresAt)
	}

	// /my/rooms lists it for the owner and nobody else.
	req, _ = http.NewRequest("GET", srv.URL+"/my/rooms", nil)
	req.AddCookie(cookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("my rooms: %v", err)
	}
	defer res.Body.Close()
	var mine struct {
		Rooms []struct {
			RoomID    string `json:"roomId"`
			Permanent bool   `json:"permanent"`
			Paired    bool   `json:"paired"`
		} `json:"rooms"`
	}
	json.NewDecoder(res.Body).Decode(&mine)
	if len(mine.Rooms) != 1 || mine.Rooms[0].RoomID != c.RoomID || !mine.Rooms[0].Permanent || !mine.Rooms[0].Paired {
		t.Fatalf("my rooms: %+v", mine.Rooms)
	}

	other := signedCookie(t, sessionUser{Sub: "user_02XYZ", Exp: time.Now().Add(time.Hour).Unix()})
	req, _ = http.NewRequest("GET", srv.URL+"/my/rooms", nil)
	req.AddCookie(other)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("my rooms other: %v", err)
	}
	defer res.Body.Close()
	var theirs struct {
		Rooms []any `json:"rooms"`
	}
	json.NewDecoder(res.Body).Decode(&theirs)
	if len(theirs.Rooms) != 0 {
		t.Errorf("other user sees rooms: %+v", theirs.Rooms)
	}
}

func TestGuestRoomUnchangedByAuth(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	// No cookie: guest room with the usual 24h ceiling and no permanent flag.
	res, err := http.Post(srv.URL+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer res.Body.Close()
	var c struct {
		RoomID    string `json:"roomId"`
		Permanent bool   `json:"permanent"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	json.NewDecoder(res.Body).Decode(&c)
	if c.Permanent {
		t.Error("guest room marked permanent")
	}
	if c.ExpiresAt == 0 || c.ExpiresAt > time.Now().Add(25*time.Hour).Unix() {
		t.Errorf("guest expiresAt=%d, want ~24h out", c.ExpiresAt)
	}
	uin, _ := normalizeUIN(c.RoomID)
	roomsMu.Lock()
	r := rooms[uin]
	roomsMu.Unlock()
	if r == nil || r.ownerID != "" || r.permanent {
		t.Errorf("guest room fields: %+v", r)
	}
	if !r.expired(time.Now().Add(25 * time.Hour)) {
		t.Error("guest room should still expire after the 24h ceiling")
	}
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestAuthCallbackLogsTransportError: with the WorkOS API unreachable the
// callback returns 502 AND logs the filtered transport error (type, cause,
// elapsed) so Railway logs carry the diagnosis. No secrets in the line.
func TestAuthCallbackLogsTransportError(t *testing.T) {
	setAuthEnv(t, "https://127.0.0.1:1") // nothing listens there: instant refusal
	srv := newTestServer(t)
	defer srv.Close()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	req, _ := http.NewRequest("GET", srv.URL+"/auth/callback?code=secret-code-xyz&state=s1", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "s1"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("callback: got %d, want 502", res.StatusCode)
	}
	line := buf.String()
	if !strings.Contains(line, "auth exchange transport failure") || !strings.Contains(line, "elapsed") {
		t.Errorf("transport error not logged: %q", line)
	}
	if strings.Contains(line, "secret-code-xyz") || strings.Contains(line, "sk_test_456") {
		t.Errorf("log line leaks the code or secret: %q", line)
	}
}
