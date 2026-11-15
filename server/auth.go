package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Optional user accounts via WorkOS AuthKit (phase 2). Guests are untouched:
// with no session cookie everything behaves exactly as before. A user who
// signs in gets permanent rooms (no expiry) tied to their WorkOS user id.
//
// Configuration (all server-side env):
//
//	WORKOS_CLIENT_ID      AuthKit client id (public, e.g. client_...)
//	WORKOS_API_KEY        WorkOS API key (secret) - used as the code-exchange
//	                      client_secret and, when ACQ_SESSION_SECRET is unset,
//	                      as the session-signing secret
//	ACQ_SESSION_SECRET    optional dedicated secret for signing session
//	                      cookies (rotate to invalidate all sessions)
//	WORKOS_API_BASE       API base URL override (tests); default
//	                      https://api.workos.com
//
// Redirect URI to register in the WorkOS dashboard:
// https://acq.damri.org/auth/callback

const (
	sessionCookie     = "acq_session"
	oauthStateCookie  = "acq_oauth_state"
	sessionLifetime   = 30 * 24 * time.Hour
	oauthStateTimeout = 10 * time.Minute
)

type authConfig struct {
	enabled    bool
	clientID   string
	apiKey     string
	apiBase    string
	sessionKey []byte
}

// authCfg reads the environment on every call: cheap, and it lets tests
// reconfigure with t.Setenv without resetting globals.
func authCfg() authConfig {
	c := authConfig{
		clientID: os.Getenv("WORKOS_CLIENT_ID"),
		apiKey:   os.Getenv("WORKOS_API_KEY"),
		apiBase:  os.Getenv("WORKOS_API_BASE"),
	}
	if c.apiBase == "" {
		c.apiBase = "https://api.workos.com"
	}
	c.enabled = c.clientID != "" && c.apiKey != ""
	secret := os.Getenv("ACQ_SESSION_SECRET")
	if secret == "" {
		secret = c.apiKey
	}
	sum := sha256.Sum256([]byte("acq-session-v1\x00" + secret))
	c.sessionKey = sum[:]
	return c
}

// sessionUser is the signed cookie payload. It is identity only - the room
// seat tokens remain the authority for reading and writing rooms.
type sessionUser struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Exp   int64  `json:"exp"`
}

func signSession(u sessionUser, key []byte) string {
	b, _ := json.Marshal(u)
	p := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySession(v string, key []byte) *sessionUser {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return nil
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(got, mac.Sum(nil)) != 1 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil
	}
	var u sessionUser
	if json.Unmarshal(b, &u) != nil || u.Sub == "" || time.Now().Unix() > u.Exp {
		return nil
	}
	return &u
}

// currentUser returns the signed-in user, or nil for guests (and whenever
// auth is not configured).
func currentUser(req *http.Request) *sessionUser {
	c := authCfg()
	if !c.enabled {
		return nil
	}
	ck, err := req.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return verifySession(ck.Value, c.sessionKey)
}

func secureCookie(req *http.Request) bool {
	return req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https"
}

// handleAuthLogin starts the AuthKit hosted flow: state cookie in, redirect
// to the WorkOS authorization URL out.
func handleAuthLogin(w http.ResponseWriter, req *http.Request) {
	c := authCfg()
	if !c.enabled {
		notAvailable(w, req)
		return
	}
	state := randToken()
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state, Path: "/auth",
		HttpOnly: true, Secure: secureCookie(req), SameSite: http.SameSiteLaxMode,
		MaxAge: int(oauthStateTimeout.Seconds()),
	})
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", baseURL(req)+"/auth/callback")
	q.Set("state", state)
	q.Set("provider", "authkit")
	http.Redirect(w, req, c.apiBase+"/user_management/authorize?"+q.Encode(), http.StatusFound)
}

// handleAuthCallback completes the flow: validates the state nonce, exchanges
// the code for the WorkOS user, and sets the signed session cookie.
func handleAuthCallback(w http.ResponseWriter, req *http.Request) {
	c := authCfg()
	if !c.enabled {
		notAvailable(w, req)
		return
	}
	fail := func(code int, msg string) {
		jsonOut(w, code, map[string]string{"detail": msg})
	}
	ck, err := req.Cookie(oauthStateCookie)
	if err != nil || ck.Value == "" || ck.Value != req.URL.Query().Get("state") {
		fail(http.StatusBadRequest, "bad oauth state")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: "", Path: "/auth",
		HttpOnly: true, MaxAge: -1,
	})
	code := req.URL.Query().Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "missing code")
		return
	}
	body, _ := json.Marshal(map[string]string{
		"client_id":     c.clientID,
		"client_secret": c.apiKey,
		"grant_type":    "authorization_code",
		"code":          code,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	res, err := client.Post(c.apiBase+"/user_management/authenticate", "application/json", bytes.NewReader(body))
	if err != nil {
		// Filtered: error type/cause and elapsed only - never the
		// authorization code, secrets, cookies, or the request body.
		// (No retry: an authorization_code is single-use, RFC 6749 4.1.2.)
		log.Printf("auth exchange transport failure: %T: %v (elapsed %s)", err, err, time.Since(start).Round(time.Millisecond))
		fail(http.StatusBadGateway, "identity provider unreachable")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fail(http.StatusUnauthorized, "authentication failed")
		return
	}
	var wr struct {
		User struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&wr); err != nil || wr.User.ID == "" {
		fail(http.StatusBadGateway, "bad identity provider response")
		return
	}
	u := sessionUser{
		Sub:   wr.User.ID,
		Email: wr.User.Email,
		Name:  strings.TrimSpace(wr.User.FirstName + " " + wr.User.LastName),
		Exp:   time.Now().Add(sessionLifetime).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: signSession(u, c.sessionKey), Path: "/",
		HttpOnly: true, Secure: secureCookie(req), SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionLifetime.Seconds()),
	})
	log.Printf("user_signed_in id=%s", u.Sub)
	http.Redirect(w, req, "/", http.StatusFound)
}

func handleAuthLogout(w http.ResponseWriter, req *http.Request) {
	if !authCfg().enabled {
		notAvailable(w, req)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: secureCookie(req), SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
	http.Redirect(w, req, "/", http.StatusFound)
}

// handleAuthMe reports the current user for the landing page. 404 when auth
// is not configured so the page can hide the widget entirely.
func handleAuthMe(w http.ResponseWriter, req *http.Request) {
	if !authCfg().enabled {
		notAvailable(w, req)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"user": currentUser(req)})
}

// handleMyRooms lists the signed-in user's permanent rooms. 404 when auth
// is not configured, like the other auth routes.
func handleMyRooms(w http.ResponseWriter, req *http.Request) {
	if !authCfg().enabled {
		notAvailable(w, req)
		return
	}
	u := currentUser(req)
	if u == nil {
		jsonOut(w, http.StatusUnauthorized, map[string]string{"detail": "sign in required"})
		return
	}
	type myRoom struct {
		RoomID    string `json:"roomId"`
		Topic     string `json:"topic,omitempty"`
		Created   int64  `json:"created"`
		Paired    bool   `json:"paired"`
		Ended     bool   `json:"ended"`
		Messages  int    `json:"messages"`
		Permanent bool   `json:"permanent"`
	}
	// Snapshot the pointers under roomsMu (ownerID is written before the
	// room enters the map and never mutated), then read the mutable fields
	// under each room's own lock - r.mu, not roomsMu, protects them.
	roomsMu.Lock()
	var owned []*Room
	for _, r := range rooms {
		if r.ownerID == u.Sub {
			owned = append(owned, r)
		}
	}
	roomsMu.Unlock()
	out := []myRoom{}
	for _, r := range owned {
		r.mu.Lock()
		out = append(out, myRoom{
			RoomID: displayUIN(r.id), Topic: r.topic, Created: r.created.Unix(),
			Paired: !r.pairedAt.IsZero(), Ended: r.ended,
			Messages: len(r.msgs), Permanent: r.permanent,
		})
		r.mu.Unlock()
	}
	jsonOut(w, http.StatusOK, map[string]any{"rooms": out})
}
