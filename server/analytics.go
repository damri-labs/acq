package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo; embed it
)

// First-party visit analytics for the landing page ("/" only), answered at
// GET /analytics to the owner alone (bearer token, see handleAnalytics).
// Cookieless and deliberately minimal: per Asia/Jerusalem day we count
// page loads (split into web-page vs agent-briefing requests) and referring
// site domains (host only, never full URLs). Daily unique visitors are
// estimated by combining each visit's IP + user-agent + date into a one-way
// hash during request handling; the IP and user-agent are never stored or
// logged by this feature, and the hashes live in memory only. Room pages
// and API endpoints are never counted.

const analyticsRetentionDays = 90

type analyticsDay struct {
	PageLoads      int            `json:"page_loads"`
	Web            int            `json:"web"`
	Agent          int            `json:"agent"`
	UniqueVisitors int            `json:"unique_visitors"`
	Referrers      map[string]int `json:"referrers"`
	// hashes estimates daily unique visitors; in memory only, never written
	// to disk or served. Nil after a restart: UniqueVisitors then keeps the
	// persisted count and may re-count that day's earlier visitors.
	hashes map[string]struct{}
}

type analyticsFile struct {
	StartedAt string                   `json:"started_at"`
	Days      map[string]*analyticsDay `json:"days"`
}

var (
	analyticsMu sync.Mutex
	analytics   = analyticsFile{Days: map[string]*analyticsDay{}}

	// analyticsLoc buckets days the way the owner reads them.
	analyticsLoc = func() *time.Location {
		l, err := time.LoadLocation("Asia/Jerusalem")
		if err != nil {
			return time.UTC
		}
		return l
	}()

	analyticsPath = func() string {
		if p := os.Getenv("ANALYTICS_FILE"); p != "" {
			return p
		}
		return "/data/analytics.json"
	}()
)

func loadAnalytics() {
	analyticsMu.Lock()
	defer analyticsMu.Unlock()
	b, err := os.ReadFile(analyticsPath)
	if err != nil {
		analytics.StartedAt = time.Now().UTC().Format(time.RFC3339)
		return
	}
	json.Unmarshal(b, &analytics)
	if analytics.StartedAt == "" {
		analytics.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if analytics.Days == nil {
		analytics.Days = map[string]*analyticsDay{}
	}
}

func persistAnalyticsLocked() {
	b, _ := json.Marshal(analytics)
	if err := os.WriteFile(analyticsPath, b, 0644); err != nil {
		log.Printf("analytics_file_error: %v", err)
	}
}

// evictAnalyticsDaysLocked drops days beyond the retention window. Date
// strings are UTC yyyy-mm-dd, so plain string comparison is chronological.
func evictAnalyticsDaysLocked() {
	cutoff := time.Now().In(analyticsLoc).AddDate(0, 0, -analyticsRetentionDays).Format("2006-01-02")
	for day := range analytics.Days {
		if day < cutoff {
			delete(analytics.Days, day)
		}
	}
}

// referrerHost reduces a Referer header to a lowercased site host; the path
// and query are discarded on purpose. Empty or unparseable means "direct".
func referrerHost(req *http.Request) string {
	r := req.Header.Get("Referer")
	if r == "" {
		return "direct"
	}
	u, err := url.Parse(r)
	if err != nil || u.Host == "" {
		return "direct"
	}
	return strings.ToLower(u.Host)
}

// recordVisit counts one landing-page visit. Called only from
// handleExplainer after the path == "/" check, so room pages, /stats, /mcp
// and every other endpoint are never counted.
//
// Enablement is explicit: collection runs only while ANALYTICS_TOKEN is
// configured. The same token gates reading, so "no token" means the feature
// is fully off - nothing is counted, persisted or served.
func recordVisit(req *http.Request) {
	if os.Getenv("ANALYTICS_TOKEN") == "" {
		return
	}
	day := time.Now().In(analyticsLoc).Format("2006-01-02")
	ref := referrerHost(req)
	h := sha256.Sum256([]byte(day + "|" + clientIP(req) + "|" + req.UserAgent()))
	key := hex.EncodeToString(h[:16])

	analyticsMu.Lock()
	defer analyticsMu.Unlock()
	d := analytics.Days[day]
	if d == nil {
		d = &analyticsDay{Referrers: map[string]int{}, hashes: map[string]struct{}{}}
		analytics.Days[day] = d
		evictAnalyticsDaysLocked()
	}
	if d.hashes == nil {
		d.hashes = map[string]struct{}{}
	}
	d.PageLoads++
	if strings.Contains(req.Header.Get("Accept"), "text/html") {
		d.Web++
	} else {
		d.Agent++
	}
	if _, seen := d.hashes[key]; !seen {
		d.hashes[key] = struct{}{}
		d.UniqueVisitors++
	}
	if len(d.Referrers) < 100 || d.Referrers[ref] > 0 || ref == "other" {
		d.Referrers[ref]++
	} else {
		d.Referrers["other"]++
	}
	persistAnalyticsLocked()
}

type analyticsDayOut struct {
	Date           string         `json:"date"`
	PageLoads      int            `json:"page_loads"`
	Web            int            `json:"web"`
	Agent          int            `json:"agent"`
	UniqueVisitors int            `json:"unique_visitors"`
	Referrers      map[string]int `json:"referrers"`
}

func handleAnalytics(w http.ResponseWriter, req *http.Request) {
	// Owner-only: disabled until ANALYTICS_TOKEN is configured, then gated
	// by it. Unknown or wrong token answers 404 like any unknown path, so
	// the endpoint's existence is not advertised.
	tok := os.Getenv("ANALYTICS_TOKEN")
	if tok == "" ||
		subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+tok)) != 1 {
		notAvailable(w, req)
		return
	}
	analyticsMu.Lock()
	days := make([]analyticsDayOut, 0, len(analytics.Days))
	for date, d := range analytics.Days {
		// Deep-copy: recordVisit writes these maps concurrently, and the
		// JSON encoding happens after the lock is released.
		refs := make(map[string]int, len(d.Referrers))
		for k, v := range d.Referrers {
			refs[k] = v
		}
		days = append(days, analyticsDayOut{
			Date:           date,
			PageLoads:      d.PageLoads,
			Web:            d.Web,
			Agent:          d.Agent,
			UniqueVisitors: d.UniqueVisitors,
			Referrers:      refs,
		})
	}
	started := analytics.StartedAt
	analyticsMu.Unlock()
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	jsonOut(w, http.StatusOK, map[string]any{
		"started_at":     started,
		"retention_days": analyticsRetentionDays,
		"note": "Visits to the landing page (/) only, counted per Asia/Jerusalem day: page loads " +
			"(web page vs agent briefing) and referring site domains (host only, never full URLs). " +
			"unique_visitors is a rough same-day estimate: each visit's IP and user-agent are " +
			"combined with the date into a one-way hash during request handling and never stored; " +
			"the hash lives in memory and is discarded, so a redeploy may re-count that day's " +
			"earlier visitors, and the IP comes from proxy headers a client can spoof. Counts " +
			"include bots, prefetchers and the owner's own test traffic - nothing here identifies " +
			"organic visitors. Room pages and API endpoints are never counted. Counting runs only " +
			"while the owner token is configured.",
		"days": days,
	})
}
