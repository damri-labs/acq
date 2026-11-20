package main

import (
	"net/http"
	"testing"
	"time"
)

func TestIndependentPracticePeerEndReleases(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	code, r := practiceCreate(t, srv.URL)
	if code != 200 {
		t.Fatal(code, r)
	}
	u := r["roomId"].(string)
	code, j := joinAsGuest(t, srv.URL, u, []byte(`{}`))
	if code != 200 {
		t.Fatal(code, j)
	}
	tok := j["token"].(string)
	if readUntil(t, srv.URL, u, tok, 5*time.Second, func(m map[string]any) bool { return m["from"] == "A" }) == nil {
		t.Fatal("no banner")
	}
	c, _ := postJSON(t, "POST", srv.URL+"/room/"+u+"/msg", tok, `{"text":"done","end":true}`)
	_ = c
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		practiceMu.Lock()
		a := practiceActive
		practiceMu.Unlock()
		if a == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	practiceMu.Lock()
	a := practiceActive
	practiceMu.Unlock()
	t.Fatalf("ended visitor room retains active bot: %d (recv on ended rooms is HTTP %d)", a, http.StatusOK)
}
func TestIndependentPracticeAmbiguousCreateDoesNotLeak(t *testing.T) {
	t.Setenv("ACQ_PRACTICE_PEER", "1")
	srv := newTestServer(t)
	defer srv.Close()
	withPersistFail(t, "post")
	code, r := practiceCreate(t, srv.URL)
	if code != 500 {
		t.Fatalf("wanted injected ambiguous failure got%d", code)
	}
	for _, key := range []string{"token", "credential", "pairCode"} {
		if _, ok := r[key]; ok {
			t.Errorf("practice ambiguous create leaks seatA field %s", key)
		}
	}
}
func TestIndependentPracticeActiveAndGlobalCaps(t *testing.T) {
	practiceMu.Lock()
	practiceCreates = map[string]int{}
	practiceTotal = 0
	practiceActive = 0
	practiceWindow = time.Now()
	practiceMu.Unlock()
	for i := 0; i < practiceMaxConcurrent; i++ {
		if !practiceAllow(string(rune('a' + i))) {
			t.Fatal("early concurrency refuse")
		}
	}
	if practiceAllow("overflow") {
		t.Fatal("concurrency not enforced")
	}
	for i := 0; i < practiceMaxConcurrent; i++ {
		practiceRelease()
	}
	practiceMu.Lock()
	practiceTotal = practiceGlobalPerHour
	practiceMu.Unlock()
	if practiceAllow("fresh") {
		t.Fatal("global hourly not enforced")
	}
}
