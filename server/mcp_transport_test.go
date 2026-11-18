package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Transport hardening for POST /mcp: Content-Type enforcement and a
// restriction-only same-origin Origin check (see handleMCP).
func mcpPost(t *testing.T, srv *httptest.Server, contentType string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mutate != nil {
		mutate(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return &httptest.ResponseRecorder{Code: res.StatusCode, HeaderMap: res.Header, Body: bytes.NewBuffer(b)}
}

func TestMCPTransportContentType(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	if r := mcpPost(t, srv, "application/json", nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"result":{}`) {
		t.Fatalf("application/json: %d %s", r.Code, r.Body.String())
	}
	if r := mcpPost(t, srv, "application/json; charset=utf-8", nil); r.Code != http.StatusOK {
		t.Fatalf("application/json with parameters: %d", r.Code)
	}
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "application/json5", ""} {
		if r := mcpPost(t, srv, ct, nil); r.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content-type %q: got %d, want 415", ct, r.Code)
		}
	}
}

func TestMCPTransportOrigin(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Absent Origin: native clients (curl, agent frameworks) pass.
	if r := mcpPost(t, srv, "application/json", nil); r.Code != http.StatusOK {
		t.Fatalf("absent Origin: %d", r.Code)
	}
	// Foreign Origin: blocked.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "https://evil.example")
	}); r.Code != http.StatusNotFound {
		t.Fatalf("foreign Origin: got %d, want 404", r.Code)
	}
	// Same-origin behind a TLS-terminating proxy (the Railway model):
	// X-Forwarded-Proto https, Origin scheme+host matching the request Host.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "https://"+req.Host)
	}); r.Code != http.StatusOK {
		t.Fatalf("same-origin via proxy: %d", r.Code)
	}
	// Scheme mismatch: Origin http while the deployment speaks https.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "http://"+req.Host)
	}); r.Code != http.StatusNotFound {
		t.Fatalf("scheme-mismatched Origin: got %d, want 404", r.Code)
	}
	// Sibling subdomain is not same-origin (strict host equality; the
	// sibling surface is not mapped, so nothing is widened to it).
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "http://sub."+req.Host)
	}); r.Code != http.StatusNotFound {
		t.Fatalf("subdomain Origin: got %d, want 404", r.Code)
	}
}

func TestMCPTransportUnchangedBehavior(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// GET stays a plain 404 (notAvailable), no Allow header added.
	res, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /mcp: got %d, want 404", res.StatusCode)
	}
	// Notifications (no id) still return bare 202, now requiring JSON type.
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: got %d, want 202", res.StatusCode)
	}
}
