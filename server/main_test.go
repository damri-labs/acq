package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestServer builds the same mux as main() and resets all global state
// between tests.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	rlMu.Lock()
	rlWindow = time.Time{}
	roomCreates = map[string]int{}
	failsByIP = map[string]int{}
	failsGlobal = 0
	rlMu.Unlock()
	tombMu.Lock()
	tombstones = map[string]time.Time{}
	tombMu.Unlock()
	penMu.Lock()
	penalties = map[string]*penalty{}
	penMu.Unlock()
	rlMu.Lock()
	feedbackByIP = map[string]int{}
	rlMu.Unlock()
	statsMu.Lock()
	stats = Stats{}
	statsMu.Unlock()
	analyticsMu.Lock()
	analytics = analyticsFile{Days: map[string]*analyticsDay{}}
	analyticsMu.Unlock()

	t.Setenv("ANALYTICS_TOKEN", "test-token")

	dir := t.TempDir()
	feedbackPath = dir + "/feedback.log"
	statsPath = dir + "/stats.json"
	analyticsPath = dir + "/analytics.json"
	roomsDir = dir + "/rooms"

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleExplainer)
	mux.HandleFunc("/robots.txt", handleRobots)
	mux.HandleFunc("/sitemap.xml", handleSitemap)
	mux.HandleFunc("/llms.txt", handleLlmsTxt)
	mux.HandleFunc("/og-card.png", handleOGCard)
	mux.HandleFunc("/room", handleCreate)
	mux.HandleFunc("/room/", handleRoom)
	mux.HandleFunc("/new", handleNew)
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/analytics", handleAnalytics)
	mux.HandleFunc("/feedback", handleFeedback)
	mux.HandleFunc("/mcp", handleMCP)
	mux.HandleFunc("/auth/login", handleAuthLogin)
	mux.HandleFunc("/auth/callback", handleAuthCallback)
	mux.HandleFunc("/auth/logout", handleAuthLogout)
	mux.HandleFunc("/auth/me", handleAuthMe)
	mux.HandleFunc("/my/rooms", handleMyRooms)
	return httptest.NewServer(mux)
}

type roomCreated struct {
	RoomID   string `json:"roomId"`
	Token    string `json:"token"`
	PairCode string `json:"pairCode"`
}

func createRoom(t *testing.T, base string) roomCreated {
	t.Helper()
	res, err := http.Post(base+"/room", "application/json", nil)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create room: got %d, want 201", res.StatusCode)
	}
	var c roomCreated
	if err := json.NewDecoder(res.Body).Decode(&c); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if c.RoomID == "" || c.Token == "" || c.PairCode == "" {
		t.Fatalf("create room: missing fields in %+v", c)
	}
	return c
}

func joinRoom(t *testing.T, base, uin string) (int, string) {
	t.Helper()
	res, err := http.Post(base+"/room/"+uin+"/join", "application/json", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Token
}

func sendMsg(t *testing.T, base, uin, token, text string, end bool) int {
	t.Helper()
	body := `{"text":` + quote(text) + `,"end":` + map[bool]string{true: "true", false: "false"}[end] + `}`
	req, _ := http.NewRequest(http.MethodPost, base+"/room/"+uin+"/msg", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func quote(s string) string { return `"` + s + `"` }

func recvMsg(t *testing.T, base, uin, token string) (int, []Msg) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/room/"+uin+"/msg?wait=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		Messages []Msg `json:"messages"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Messages
}

func TestRoomLifecycle(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)

	// Seat B joins once.
	code, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusOK || tokB == "" {
		t.Fatalf("join: got %d token=%q, want 200 with token", code, tokB)
	}

	// Second join is rejected - the UIN is a one-time ticket.
	code, _ = joinRoom(t, srv.URL, c.RoomID)
	if code != http.StatusConflict {
		t.Fatalf("second join: got %d, want 409", code)
	}

	// A sends, B receives.
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "uh-oh!", false); code != http.StatusOK {
		t.Fatalf("send: got %d, want 200", code)
	}
	code, msgs := recvMsg(t, srv.URL, c.RoomID, tokB)
	if code != http.StatusOK {
		t.Fatalf("recv: got %d, want 200", code)
	}
	if len(msgs) != 1 || msgs[0].Text != "uh-oh!" || msgs[0].From != "A" {
		t.Fatalf("recv: got %+v, want one message 'uh-oh!' from A", msgs)
	}

	// B replies and ends the room; further sends are rejected.
	if code := sendMsg(t, srv.URL, c.RoomID, tokB, "bye", true); code != http.StatusOK {
		t.Fatalf("end send: got %d, want 200", code)
	}
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "still here?", false); code != http.StatusGone {
		t.Fatalf("send after end: got %d, want 410", code)
	}
}

func TestVerifyLockout(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)

	verify := func(code string) bool {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/verify",
			strings.NewReader(`{"code":"`+code+`"}`))
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Match bool `json:"match"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		return body.Match
	}

	// Five wrong attempts burn the allowance.
	for i := 0; i < 5; i++ {
		if verify("000000") {
			t.Fatalf("attempt %d: wrong code matched", i+1)
		}
	}
	// The lockout is permanent: even the correct code no longer matches.
	if verify(c.PairCode) {
		t.Fatal("after 5 failures the correct code still matched; lockout broken")
	}
}

func TestVerifyCorrectCode(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room/"+c.RoomID+"/verify",
		strings.NewReader(`{"code":"`+c.PairCode+`"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		Match bool `json:"match"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	if !body.Match {
		t.Fatal("correct pair code did not match")
	}
}

func TestClientIPTrustsLastXFFValue(t *testing.T) {
	// Behind Railway's proxy the last XFF value is the one the proxy added;
	// earlier values are client-controlled and must not be trusted.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1, 203.0.113.9")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("clientIP with spoofed XFF: got %q, want %q (last value)", got, "203.0.113.9")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := clientIP(req); got != "198.51.100.7" {
		t.Fatalf("clientIP single XFF: got %q, want %q", got, "198.51.100.7")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.55:44321"
	if got := clientIP(req); got != "192.0.2.55" {
		t.Fatalf("clientIP no XFF: got %q, want RemoteAddr host %q", got, "192.0.2.55")
	}
}

func TestSpoofedXFFDoesNotBypassRateLimit(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// One real client (same last XFF value) rotating spoofed leading values
	// must still hit the per-IP room-creation cap.
	last := "203.0.113.99"
	allowed, limited := 0, 0
	for i := 0; i < roomsPerIPHour+2; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/room", nil)
		req.Header.Set("X-Forwarded-For", "1.1.1."+string(rune('0'+i%10))+", "+last)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusCreated {
			allowed++
		} else if res.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if allowed > roomsPerIPHour {
		t.Fatalf("spoofed XFF bypassed the cap: %d rooms allowed, cap is %d", allowed, roomsPerIPHour)
	}
	if limited == 0 {
		t.Fatal("rate limit never triggered despite exceeding the cap")
	}
}

func TestExplainerContentNegotiation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// curl (no Accept: text/html) gets the plain-text API docs.
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("default Accept: got content-type %q, want text/plain", ct)
	}

	// Browsers get the landing page.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get html: %v", err)
	}
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Accept html: got content-type %q, want text/html", ct)
	}
}

func TestStatsCounter(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "hi", false); code != http.StatusOK {
		t.Fatalf("send: got %d, want 200", code)
	}
	_ = tokB

	res, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stats: got %d, want 200", res.StatusCode)
	}
	var s Stats
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if s.RoomsCreated != 1 || s.RoomsPaired != 1 || s.Messages != 1 {
		t.Fatalf("stats: got %+v, want 1 room created, 1 paired, 1 message", s)
	}
}

func TestStatsPersistAcrossReload(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	createRoom(t, srv.URL)

	// Simulate a restart: wipe the in-memory counters, reload from disk.
	statsMu.Lock()
	stats = Stats{}
	statsMu.Unlock()
	loadStats()

	statsMu.Lock()
	got := stats.RoomsCreated
	statsMu.Unlock()
	if got != 1 {
		t.Fatalf("after reload: rooms_created=%d, want 1 (persisted)", got)
	}
}

func TestPublicFeedback(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	post := func(text string) int {
		res, err := http.Post(srv.URL+"/feedback", "application/json",
			strings.NewReader(`{"text":`+quote(text)+`,"purpose":"test"}`))
		if err != nil {
			t.Fatalf("feedback post: %v", err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	if code := post("great service"); code != http.StatusOK {
		t.Fatalf("valid feedback: got %d, want 200", code)
	}
	if code := post("   "); code != http.StatusBadRequest {
		t.Fatalf("blank feedback: got %d, want 400", code)
	}
	// Cap is 5/hour per IP; one valid + one rejected-so-far... blank posts
	// still consume the allowance, so post until the limiter trips.
	limited := false
	for i := 0; i < feedbackPerIPHour+2; i++ {
		if post("again") == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("public feedback was never rate-limited")
	}

	// The accepted entries landed in the feedback log marked source=web.
	feedbackMu.Lock()
	b, err := os.ReadFile(feedbackPath)
	feedbackMu.Unlock()
	if err != nil {
		t.Fatalf("read feedback log: %v", err)
	}
	if !strings.Contains(string(b), `"source":"web"`) || !strings.Contains(string(b), "great service") {
		t.Fatalf("feedback log missing web entry: %q", b)
	}
}

func TestExplainerBriefing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	body := string(b)
	for _, want := range []string{"WHAT ACQ IS", "FULL FLOW", "STATS", "/feedback", "FEEDBACK"} {
		if !strings.Contains(body, want) {
			t.Fatalf("explainer missing %q", want)
		}
	}
}

func TestRoomPersistenceAcrossRestart(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if code := sendMsg(t, srv.URL, c.RoomID, c.Token, "persist me", false); code != http.StatusOK {
		t.Fatalf("send: got %d, want 200", code)
	}

	// Simulate a redeploy: wipe in-memory state, reload from disk.
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()

	// The room survives: B receives the pending message.
	code, msgs := recvMsg(t, srv.URL, c.RoomID, tokB)
	if code != http.StatusOK {
		t.Fatalf("recv after restart: got %d, want 200", code)
	}
	if len(msgs) != 1 || msgs[0].Text != "persist me" {
		t.Fatalf("recv after restart: got %+v, want the pre-restart message", msgs)
	}
	// The UIN is still burned: a second join is rejected.
	if code, _ := joinRoom(t, srv.URL, c.RoomID); code != http.StatusConflict {
		t.Fatalf("second join after restart: got %d, want 409", code)
	}
	// And the room file exists on disk (named by the raw id, no dashes).
	raw := strings.ReplaceAll(c.RoomID, "-", "")
	if _, err := os.Stat(roomsDir + "/" + raw + ".json"); err != nil {
		t.Fatalf("room file missing after reload: %v", err)
	}
}

// mcpRPC sends one JSON-RPC call to the test server's /mcp and returns the
// decoded result object.
func mcpRPC(t *testing.T, base, method string, params map[string]any, id int) map[string]any {
	t.Helper()
	reqBody := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		reqBody["params"] = params
	}
	b, _ := json.Marshal(reqBody)
	res, err := http.Post(base+"/mcp", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("mcp %s: %v", method, err)
	}
	defer res.Body.Close()
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("mcp %s decode: %v", method, err)
	}
	if resp.Error != nil {
		t.Fatalf("mcp %s: rpc error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("mcp %s result decode: %v", method, err)
	}
	return out
}

// mcpToolText runs tools/call and returns the first text content.
func mcpToolText(t *testing.T, base, tool string, args map[string]any, id int) string {
	t.Helper()
	out := mcpRPC(t, base, "tools/call", map[string]any{"name": tool, "arguments": args}, id)
	if out["isError"] == true {
		t.Fatalf("tool %s returned error: %v", tool, out)
	}
	content, _ := out["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool %s: empty content", tool)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func TestMCPInitializeAndTools(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	init := mcpRPC(t, srv.URL, "initialize", nil, 1)
	info, _ := init["serverInfo"].(map[string]any)
	if info["name"] != "acq" {
		t.Fatalf("initialize: serverInfo=%v", info)
	}

	list := mcpRPC(t, srv.URL, "tools/list", nil, 2)
	tools, _ := list["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"acq_create_room", "acq_join_room", "acq_rejoin", "acq_send", "acq_recv", "acq_feedback", "acq_stats"} {
		if !names[want] {
			t.Fatalf("tools/list missing %s (has %v)", want, names)
		}
	}
}

func TestMCPRoomFlow(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	created := mcpToolText(t, srv.URL, "acq_create_room", map[string]any{}, 1)
	var c roomCreated
	if err := json.Unmarshal([]byte(created), &c); err != nil {
		t.Fatalf("create result not JSON: %q", created)
	}
	if c.RoomID == "" || c.Token == "" {
		t.Fatalf("create result missing fields: %q", created)
	}

	joined := mcpToolText(t, srv.URL, "acq_join_room", map[string]any{"uin": c.RoomID}, 2)
	var j struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(joined), &j); err != nil || j.Token == "" {
		t.Fatalf("join result: %q err=%v", joined, err)
	}

	mcpToolText(t, srv.URL, "acq_send", map[string]any{"uin": c.RoomID, "token": c.Token, "text": "hello over mcp"}, 3)

	got := mcpToolText(t, srv.URL, "acq_recv", map[string]any{"uin": c.RoomID, "token": j.Token, "wait": 0}, 4)
	if !strings.Contains(got, "hello over mcp") {
		t.Fatalf("recv result missing message: %q", got)
	}

	stats := mcpToolText(t, srv.URL, "acq_stats", map[string]any{}, 5)
	if !strings.Contains(stats, `"rooms_created":1`) {
		t.Fatalf("stats after mcp flow: %q", stats)
	}
}

// TestMCPCreateRoomUsesRequestHost is a regression test: rooms created via the
// MCP tools/call surface used to embed "https://example.com/..." invite and
// share links, because the internal sub-request was built with
// httptest.NewRequest, whose default Host is "example.com". The external
// request's Host and X-Forwarded-Proto must be threaded through instead.
func TestMCPCreateRoomUsesRequestHost(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"acq_create_room","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	if len(rpc.Result.Content) == 0 {
		t.Fatal("no content in MCP tools/call result")
	}
	var created struct {
		Share        string `json:"share"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &created); err != nil {
		t.Fatalf("create_room result is not a create response: %v", err)
	}
	blob := created.Share + "\n" + created.Instructions
	if strings.Contains(blob, "example.com") {
		t.Fatalf("MCP-created room links point at example.com:\n%s", blob)
	}
	if want := "https://" + host + "/room/"; !strings.Contains(created.Share, want) {
		t.Fatalf("share line missing %q:\n%s", want, created.Share)
	}
}

// TestCanonicalBaseURLIgnoresHostHeaders: with ACQ_PUBLIC_BASE_URL set (as the
// production image does), generated links must use the configured origin even
// when the client sends a hostile Host / X-Forwarded-Proto - on both the REST
// and the MCP surface.
func TestCanonicalBaseURLIgnoresHostHeaders(t *testing.T) {
	t.Setenv("ACQ_PUBLIC_BASE_URL", "https://acq.damri.org")
	ts := newTestServer(t)
	defer ts.Close()

	// REST create with a hostile Host header.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/room", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		Share string `json:"share"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.Share, "https://acq.damri.org/room/") {
		t.Fatalf("REST share line missing canonical base:\n%s", created.Share)
	}
	if strings.Contains(created.Share, "evil.example") {
		t.Fatalf("REST share line poisoned by Host header:\n%s", created.Share)
	}

	// MCP create with a hostile Host header.
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"acq_create_room","arguments":{}}}`
	mreq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	mreq.Host = "evil.example"
	mreq.Header.Set("Content-Type", "application/json")
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(mresp.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	var mcreated struct {
		Share string `json:"share"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &mcreated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcreated.Share, "https://acq.damri.org/room/") {
		t.Fatalf("MCP share line missing canonical base:\n%s", mcreated.Share)
	}
	if strings.Contains(mcreated.Share, "evil.example") {
		t.Fatalf("MCP share line poisoned by Host header:\n%s", mcreated.Share)
	}
}
