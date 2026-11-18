package main

import (
	"net/http"
	"strings"
	"testing"
)

// Regression coverage for branches of the POST /mcp hardening contract that
// mcp_transport_test.go does not exercise: the malformed-Origin parse-error
// path, the opaque ("null") browser Origin, the direct-connection scheme
// fallback in trustedScheme, the ACQ_PUBLIC_SCHEME deployment override,
// strict host:port equality, Content-Type normalization, and the no-CORS
// preflight property the threat model relies on.
func TestMCPHardeningRegression(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Opaque Origin: sandboxed iframes and data: URLs send the literal
	// "null". It parses as a relative URL (empty scheme), so it must be
	// rejected by the scheme check.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "null")
	}); r.Code != http.StatusNotFound {
		t.Fatalf("null Origin: got %d, want 404", r.Code)
	}

	// Malformed Origin: url.Parse fails, the err branch must reject.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "http://[::1")
	}); r.Code != http.StatusNotFound {
		t.Fatalf("malformed Origin: got %d, want 404", r.Code)
	}

	// Same host, different port is a different browser origin and must not
	// pass strict host equality.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		host, _, _ := strings.Cut(req.Host, ":")
		req.Header.Set("Origin", "http://"+host+":1")
	}); r.Code != http.StatusNotFound {
		t.Fatalf("different-port Origin: got %d, want 404", r.Code)
	}

	// Direct connection, no proxy headers: trustedScheme falls back to
	// http, so a matching plain-http Origin passes.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "http://"+req.Host)
	}); r.Code != http.StatusOK {
		t.Fatalf("same-origin direct http: got %d, want 200", r.Code)
	}

	// A foreign Origin is still rejected when it spoofs
	// X-Forwarded-Proto: the check only ever restricts.
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "https://evil.example")
	}); r.Code != http.StatusNotFound {
		t.Fatalf("foreign Origin with spoofed XFP: got %d, want 404", r.Code)
	}

	// Content-Type normalization: media type is case-insensitive per RFC
	// 2045 and must still parse as application/json.
	if r := mcpPost(t, srv, "Application/JSON", nil); r.Code != http.StatusOK {
		t.Fatalf("case-normalized Content-Type: got %d, want 200", r.Code)
	}

	// No CORS preflight is answered: OPTIONS must get the same 404 as any
	// non-POST method, with no Access-Control-Allow-Origin header. The
	// hardening depends on browsers being unable to send a cross-origin
	// application/json request past preflight.
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/mcp", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("OPTIONS preflight: got %d, want 404", res.StatusCode)
	}
	if h := res.Header.Get("Access-Control-Allow-Origin"); h != "" {
		t.Fatalf("OPTIONS preflight answered with Access-Control-Allow-Origin: %q", h)
	}
}

// The deployment override pins the advertised scheme regardless of proxy
// headers; a matching Origin under the override passes and a
// scheme-mismatched one fails.
func TestMCPHardeningPublicSchemeOverride(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	t.Setenv("ACQ_PUBLIC_SCHEME", "https")

	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "https://"+req.Host)
	}); r.Code != http.StatusOK {
		t.Fatalf("override https, https Origin: got %d, want 200", r.Code)
	}
	if r := mcpPost(t, srv, "application/json", func(req *http.Request) {
		req.Header.Set("Origin", "http://"+req.Host)
	}); r.Code != http.StatusNotFound {
		t.Fatalf("override https, http Origin: got %d, want 404", r.Code)
	}
}
