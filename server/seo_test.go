package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSEOEndpoints: robots.txt, sitemap.xml and the og card are served with
// the right content types and expected markers, and the landing head carries
// the canonical/og/twitter meta block.
func TestSEOEndpoints(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()

	get := func(path string) (int, string, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), string(b)
	}

	code, ct, body := get("/robots.txt")
	if code != http.StatusOK || !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("robots: got %d %q", code, ct)
	}
	if !strings.Contains(body, "Sitemap: https://acq.damri.org/sitemap.xml") {
		t.Fatalf("robots missing sitemap line: %q", body)
	}

	code, ct, body = get("/sitemap.xml")
	if code != http.StatusOK || !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("sitemap: got %d %q", code, ct)
	}
	if !strings.Contains(body, "<loc>https://acq.damri.org/</loc>") {
		t.Fatalf("sitemap missing root loc: %q", body)
	}

	code, ct, body = get("/llms.txt")
	if code != http.StatusOK || !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("llms.txt: got %d %q", code, ct)
	}
	for _, marker := range []string{"# ACQ", "https://acq.damri.org/mcp", "no authority", "Instinct, an AI agent acting on behalf of Tal Chaim Damri", "The relay can read text", "No wake or orchestration"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("llms.txt missing %q", marker)
		}
	}

	code, ct, body = get("/og-card.png")
	if code != http.StatusOK || ct != "image/png" {
		t.Fatalf("og-card: got %d %q", code, ct)
	}
	if !strings.HasPrefix(body, "\x89PNG") {
		t.Fatalf("og-card is not a PNG")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("landing: %v", err)
	}
	defer res.Body.Close()
	lb, _ := io.ReadAll(res.Body)
	for _, marker := range []string{
		`<link rel="canonical" href="https://acq.damri.org/">`,
		`<meta property="og:title" content="ACQ - ICQ for AI agents">`,
		`<meta property="og:image" content="https://acq.damri.org/og-card.png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
	} {
		if !strings.Contains(string(lb), marker) {
			t.Fatalf("landing head missing %s", marker)
		}
	}
}
