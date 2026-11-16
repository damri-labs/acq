package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// getLanding issues a GET to "/" with the given headers and returns the status.
func getLanding(t *testing.T, base, accept, referer, xff, ua string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/", nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func fetchAnalytics(t *testing.T, base string) (int, map[string]any) {
	t.Helper()
	t.Setenv("ANALYTICS_TOKEN", "test-token")
	req, _ := http.NewRequest("GET", base+"/analytics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /analytics: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode /analytics: %v", err)
	}
	return res.StatusCode, out
}

// TestAnalyticsOwnerOnly: without ANALYTICS_TOKEN configured the endpoint
// answers 404 like any unknown path; a wrong token gets the same; only the
// configured bearer token opens it.
func TestAnalyticsOwnerOnly(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	get := func(auth string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/analytics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /analytics: %v", err)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}

	t.Setenv("ANALYTICS_TOKEN", "")
	if code := get(""); code != http.StatusNotFound {
		t.Fatalf("unset token: expected 404, got %d", code)
	}
	t.Setenv("ANALYTICS_TOKEN", "test-token")
	if code := get(""); code != http.StatusNotFound {
		t.Fatalf("no header: expected 404, got %d", code)
	}
	if code := get("Bearer wrong"); code != http.StatusNotFound {
		t.Fatalf("wrong token: expected 404, got %d", code)
	}
	if code := get("Bearer test-token"); code != http.StatusOK {
		t.Fatalf("right token: expected 200, got %d", code)
	}
}

func analyticsDays(t *testing.T, base string) []any {
	t.Helper()
	_, out := fetchAnalytics(t, base)
	days, ok := out["days"].([]any)
	if !ok {
		t.Fatalf("/analytics days missing or wrong type: %v", out)
	}
	return days
}

// TestAnalyticsCountsWebVsAgent: a browser-style request and a curl-style
// request are counted separately on the same UTC day.
func TestAnalyticsCountsWebVsAgent(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	if code := getLanding(t, srv.URL, "text/html", "", "1.2.3.4", "TestBrowser/1.0"); code != http.StatusOK {
		t.Fatalf("landing web: got %d", code)
	}
	if code := getLanding(t, srv.URL, "", "", "5.6.7.8", "curl/8.0"); code != http.StatusOK {
		t.Fatalf("landing agent: got %d", code)
	}

	days := analyticsDays(t, srv.URL)
	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(days))
	}
	d := days[0].(map[string]any)
	if d["page_loads"].(float64) != 2 || d["web"].(float64) != 1 || d["agent"].(float64) != 1 {
		t.Fatalf("bad counts: %v", d)
	}
	if d["unique_visitors"].(float64) != 2 {
		t.Fatalf("expected 2 unique visitors, got %v", d["unique_visitors"])
	}
	wantDay := time.Now().In(analyticsLoc).Format("2006-01-02")
	if d["date"].(string) != wantDay {
		t.Fatalf("day bucket %v, want Asia/Jerusalem day %s", d["date"], wantDay)
	}
}

// TestAnalyticsUniqueVisitorEstimate: same IP+UA twice counts as one unique
// visitor but two page loads; a changed user-agent counts as a new one.
func TestAnalyticsUniqueVisitorEstimate(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	getLanding(t, srv.URL, "", "", "1.2.3.4", "curl/8.0")
	getLanding(t, srv.URL, "", "", "1.2.3.4", "curl/8.0")
	getLanding(t, srv.URL, "", "", "1.2.3.4", "curl/9.0")

	d := analyticsDays(t, srv.URL)[0].(map[string]any)
	if d["page_loads"].(float64) != 3 {
		t.Fatalf("expected 3 page loads, got %v", d["page_loads"])
	}
	if d["unique_visitors"].(float64) != 2 {
		t.Fatalf("expected 2 unique visitors, got %v", d["unique_visitors"])
	}
}

// TestAnalyticsReferrerHostOnly: only the referring site's host is kept,
// lowercased; path and query are discarded. No Referer means "direct".
func TestAnalyticsReferrerHostOnly(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	getLanding(t, srv.URL, "", "https://News.YCombinator.COM/item?id=12345", "1.2.3.4", "a")
	getLanding(t, srv.URL, "", "", "1.2.3.4", "b")

	d := analyticsDays(t, srv.URL)[0].(map[string]any)
	refs := d["referrers"].(map[string]any)
	if refs["news.ycombinator.com"].(float64) != 1 {
		t.Fatalf("referrer host not reduced/lowercased: %v", refs)
	}
	if refs["direct"].(float64) != 1 {
		t.Fatalf("missing direct bucket: %v", refs)
	}
	for k := range refs {
		if strings.Contains(k, "/") || strings.Contains(k, "?") || strings.Contains(k, "id=") {
			t.Fatalf("referrer kept path/query: %q", k)
		}
	}
}

// TestAnalyticsNeverCountsRoomsOrAPI: room pages, room creation, /stats,
// /feedback and unknown paths leave no analytics trace.
func TestAnalyticsNeverCountsRoomsOrAPI(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	rc := createRoom(t, srv.URL)
	res, _ := http.Get(srv.URL + "/room/" + rc.RoomID)
	if res != nil {
		res.Body.Close()
	}
	req, _ := http.NewRequest("GET", srv.URL+"/room/"+rc.RoomID, nil)
	req.Header.Set("Accept", "text/html")
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
	}
	for _, p := range []string{"/stats", "/analytics", "/new", "/robots.txt", "/nonexistent"} {
		if res, err := http.Get(srv.URL + p); err == nil {
			res.Body.Close()
		}
	}

	if days := analyticsDays(t, srv.URL); len(days) != 0 {
		t.Fatalf("non-landing requests were counted: %v", days)
	}
}

// TestAnalyticsPersistsAggregatesOnly: the JSON file holds daily aggregates
// and no per-visitor material (no hashes, IPs or user-agents).
func TestAnalyticsPersistsAggregatesOnly(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	getLanding(t, srv.URL, "text/html", "https://example.com/page?x=1", "9.9.9.9", "TestBrowser/1.0")

	b, err := os.ReadFile(analyticsPath)
	if err != nil {
		t.Fatalf("analytics file not written: %v", err)
	}
	for _, leak := range []string{"9.9.9.9", "TestBrowser", "hashes", "example.com/page"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("analytics file leaks %q: %s", leak, b)
		}
	}
	var f analyticsFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("analytics file does not parse: %v", err)
	}
	var total int
	for _, d := range f.Days {
		total += d.PageLoads
	}
	if total != 1 {
		t.Fatalf("expected 1 persisted page load, got %d", total)
	}
}

// TestAnalyticsEviction: days older than the retention window are dropped on
// the next recorded visit.
func TestAnalyticsEviction(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	analyticsMu.Lock()
	analytics.Days["2020-01-01"] = &analyticsDay{PageLoads: 5, Referrers: map[string]int{}}
	analyticsMu.Unlock()

	getLanding(t, srv.URL, "", "", "1.2.3.4", "a")

	analyticsMu.Lock()
	_, old := analytics.Days["2020-01-01"]
	analyticsMu.Unlock()
	if old {
		t.Fatal("day beyond retention was not evicted")
	}
}

// TestAnalyticsEndpointShape: /analytics answers with the documented fields.
func TestAnalyticsEndpointShape(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	getLanding(t, srv.URL, "text/html", "", "1.2.3.4", "a")

	_, out := fetchAnalytics(t, srv.URL)
	for _, k := range []string{"started_at", "retention_days", "note", "days"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("/analytics missing %q: %v", k, out)
		}
	}
	if out["retention_days"].(float64) != analyticsRetentionDays {
		t.Fatalf("retention_days: %v", out["retention_days"])
	}
}

// TestAnalyticsConcurrentVisitAndRead: hammer recordVisit and handleAnalytics
// together; run with -race. Regression for the shallow-copy data race where
// the handler encoded a live Referrers map after releasing the lock.
func TestAnalyticsConcurrentVisitAndRead(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	t.Setenv("ANALYTICS_TOKEN", "test-token")

	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(n int) {
			for i := 0; i < 50; i++ {
				req, _ := http.NewRequest("GET", srv.URL+"/", nil)
				req.Header.Set("Referer", "https://example-"+string(rune('a'+n))+".com/x")
				if res, err := http.DefaultClient.Do(req); err == nil {
					res.Body.Close()
				}
			}
			done <- struct{}{}
		}(w)
	}
	for r := 0; r < 2; r++ {
		go func() {
			for i := 0; i < 50; i++ {
				req, _ := http.NewRequest("GET", srv.URL+"/analytics", nil)
				req.Header.Set("Authorization", "Bearer test-token")
				res, err := http.DefaultClient.Do(req)
				if err == nil {
					var out map[string]any
					json.NewDecoder(res.Body).Decode(&out)
					res.Body.Close()
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 6; i++ {
		<-done
	}
}

// TestAnalyticsCollectionGatedOnToken: with ANALYTICS_TOKEN unset, visits to
// "/" record nothing - "endpoint disabled" means collection disabled too.
func TestAnalyticsCollectionGatedOnToken(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	t.Setenv("ANALYTICS_TOKEN", "")

	getLanding(t, srv.URL, "text/html", "https://example.com/x", "1.2.3.4", "a")
	getLanding(t, srv.URL, "", "", "1.2.3.4", "b")

	analyticsMu.Lock()
	n := len(analytics.Days)
	analyticsMu.Unlock()
	if n != 0 {
		t.Fatalf("visits recorded while disabled: %v days", n)
	}
}
