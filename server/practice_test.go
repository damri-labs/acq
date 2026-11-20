package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func practiceCreate(t *testing.T, base string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(base+"/practice", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("practice create: %v", err)
	}
	defer res.Body.Close()
	var resp map[string]any
	json.NewDecoder(res.Body).Decode(&resp)
	return res.StatusCode, resp
}

func sendText(t *testing.T, base, uin, token, text string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/room/"+uin+"/msg",
		bytes.NewReader([]byte(`{"text":`+strconv.Quote(text)+`,"end":false}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("send: got %d", res.StatusCode)
	}
}

// readUntil polls seat B until pred matches a message or the deadline passes.
func readUntil(t *testing.T, base, uin, token string, timeout time.Duration, pred func(m map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	since := 0
	for time.Now().Before(deadline) {
		code, out := getJSON(t, base+"/room/"+uin+"/msg?since="+itoa(since)+"&wait=3", token)
		if code != http.StatusOK {
			continue
		}
		if c, ok := out["cursor"].(float64); ok {
			since = int(c)
		}
		msgs, _ := out["messages"].([]any)
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if pred(mm) {
				return mm
			}
		}
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

// With the env gate unset, no /practice route exists at all.
func TestPracticeRouteAbsentWhenDisabled(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	code, _ := practiceCreate(t, srv.URL)
	if code != http.StatusNotFound {
		t.Fatalf("disabled practice: got %d, want 404", code)
	}
}

// The practice response must not leak the bot's seat token or pair code.
func TestPracticeCreateShape(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	code, resp := practiceCreate(t, srv.URL)
	if code != http.StatusOK {
		t.Fatalf("practice create: got %d, want 200 (%v)", code, resp)
	}
	if resp["roomId"] == "" || resp["practice"] != true {
		t.Fatalf("practice create: bad shape %+v", resp)
	}
	for _, secret := range []string{"token", "credential", "pairCode"} {
		if _, leaked := resp[secret]; leaked {
			t.Fatalf("practice create leaks %s: %+v", secret, resp)
		}
	}
}

// Full loop: join, banner, five scripted replies, room ends. The bot's
// replies are the constant script lines and never echo visitor content.
func TestPracticeExchange(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()

	_, resp := practiceCreate(t, srv.URL)
	uin, _ := resp["roomId"].(string)
	if uin == "" {
		t.Fatalf("no roomId in %+v", resp)
	}
	code, jr := joinAsGuest(t, srv.URL, uin, []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("join: got %d, want 200", code)
	}
	tokB, _ := jr["token"].(string)

	banner := readUntil(t, srv.URL, uin, tokB, 15*time.Second, func(m map[string]any) bool {
		tx, _ := m["text"].(string)
		return m["from"] == "A" && strings.Contains(tx, "deterministic demo bot")
	})
	if banner == nil {
		t.Fatal("no identity banner from practice bot")
	}

	secretProbe := "visitor-secret-marker-zz9"
	for i := 1; i <= 5; i++ {
		sendText(t, srv.URL, uin, tokB, secretProbe)
		want := "Reply " + string(rune('0'+i)) + "/5"
		reply := readUntil(t, srv.URL, uin, tokB, 15*time.Second, func(m map[string]any) bool {
			tx, _ := m["text"].(string)
			return m["from"] == "A" && strings.Contains(tx, want)
		})
		if reply == nil {
			t.Fatalf("no scripted reply %d", i)
		}
		tx, _ := reply["text"].(string)
		if strings.Contains(tx, secretProbe) {
			t.Fatalf("bot echoed visitor content in reply %d", i)
		}
		if i == 5 && reply["end"] != true {
			t.Fatalf("final reply did not end the room: %+v", reply)
		}
	}
}

// Per-IP practice limit keys on the TCP peer (trusted), never on the
// spoofable X-Forwarded-For header: a caller at the limit stays refused even
// when presenting a fresh forwarded identity per request.
func TestPracticePerIPLimit(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	practiceMu.Lock()
	practiceCreates["127.0.0.1"] = practicePerIPHour
	practiceWindow = time.Now()
	practiceMu.Unlock()
	for _, xff := range []string{"", "203.0.113.7", "198.51.100.9"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/practice", bytes.NewReader([]byte(`{}`)))
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("over-limit practice create: %v", err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("over-limit practice create (XFF %q): got %d, want 429", xff, res.StatusCode)
		}
	}
}

// A create that fails ambiguously after the room committed (post-rename
// persist fault) must answer an honest 500 with a static detail: the
// upstream body rides the fresh room's seat token, credential and pair
// code, none of which may reach the visitor - nor the UIN, which would let
// the visitor join a room that has no bot.
func TestPracticeCreateFailureLeaksNothing(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	withPersistFail(t, "post")
	code, resp := practiceCreate(t, srv.URL)
	if code != http.StatusInternalServerError {
		t.Fatalf("fault-injected practice create: got %d, want 500", code)
	}
	for _, secret := range []string{"token", "credential", "pairCode", "roomId", "stored"} {
		if _, leaked := resp[secret]; leaked {
			t.Fatalf("fault path leaks %s: %+v", secret, resp)
		}
	}
	if resp["detail"] != "practice room creation failed" {
		t.Fatalf("fault path detail: %+v", resp)
	}
}

// A GET on /practice must never mint a room.
func TestPracticeGetDoesNotMint(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/practice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /practice: got %d, want 405", res.StatusCode)
	}
}

// An ordinary room whose caller chose a topic matching the practice bot's
// topic must survive process boot untouched: topic is untrusted metadata,
// never ownership provenance, so no startup path may end rooms by topic.
func TestPracticeBootPreservesOrdinaryMatchingTopic(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	code, resp := postJSON(t, "POST", srv.URL+"/room", "", `{"topic":"practice with ACQ Practice (deterministic demo bot)"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create matching-topic room: got %d", code)
	}
	uin := resp["roomId"].(string)
	token := resp["token"].(string)
	loadRooms() // simulate a process restart against the persisted store
	code, _ = postJSON(t, "POST", srv.URL+"/room/"+uin+"/msg", token, `{"text":"still mine","end":false}`)
	if code != http.StatusOK {
		t.Fatalf("ordinary matching-topic room harmed across boot: send got %d, want 200", code)
	}
}
