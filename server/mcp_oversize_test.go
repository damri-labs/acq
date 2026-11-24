package main

// Regression: the MCP JSON-RPC envelope budget and oversized-message error.
// A legitimate large message must fit the transport; an over-cap message
// must surface a readable error naming the cap, never a misleading -32700
// parse error on a truncated body. The 64 KB message cap itself is
// preserved.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// mcpBigCall issues an acq_send tools/call over MCP for a fixed room and
// returns the HTTP status and decoded response body.
func mcpBigCall(t *testing.T, base, room, token, text string) (int, map[string]any) {
	t.Helper()
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"acq_send","arguments":{"uin":%q,"token":%q,"text":%q}}}`, room, token, text)
	resp, err := http.Post(base+"/mcp", "application/json", bytes.NewReader([]byte(call)))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func transcriptCount(t *testing.T, base, room, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/room/"+room+"/msg?self=1&peek=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /msg: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return len(out.Messages)
}

// A legitimate large message (65,400 bytes of text - under the 65,536 wire cap, but its JSON envelope exceeds the old 65,536-byte read budget) fits the wire cap and
// must succeed through the MCP transport - the envelope headroom bug
// previously rejected it with a misleading parse error.
func TestMCPLegitLargeMessage(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, body := mcpBigCall(t, srv.URL, c.RoomID, c.Token, strings.Repeat("x", 65400))
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	res, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no tool result in response: %v", body)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("legit 64KB-class message rejected: %v", res)
	}
	if n := transcriptCount(t, srv.URL, c.RoomID, c.Token); n != 1 {
		t.Fatalf("transcript count = %d, want 1", n)
	}
}

// The message cap itself is preserved: text over 65,536 bytes reaches the
// send handler and comes back as a readable tool error naming the cap, not
// a protocol failure or a terse "bad message".
func TestMCPMessageCapPreserved(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, body := mcpBigCall(t, srv.URL, c.RoomID, c.Token, strings.Repeat("x", 65537))
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	res, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("over-cap text did not reach the handler: %v", body)
	}
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("over-cap text accepted: %v", res)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 || !strings.Contains(fmt.Sprint(content[0]), "65536") {
		t.Fatalf("expected cap-naming error, got %v", res)
	}
	if n := transcriptCount(t, srv.URL, c.RoomID, c.Token); n != 0 {
		t.Fatalf("over-cap message committed: transcript count = %d", n)
	}
}

// An envelope over the transport budget gets a readable, protocol-shaped
// 413 that names the 64 KB cap - not a misleading -32700 parse error on a
// truncated body - and commits nothing.
func TestMCPOversizeEnvelopeReadable(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	code, body := mcpBigCall(t, srv.URL, c.RoomID, c.Token, strings.Repeat("x", maxRequestBytes+1))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", code)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no protocol error object in response: %v", body)
	}
	if rc, _ := errObj["code"].(float64); rc != -32600 {
		t.Fatalf("error code = %v, want -32600", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "65536") {
		t.Fatalf("error message does not name the 64KB cap: %q", msg)
	}
	if n := transcriptCount(t, srv.URL, c.RoomID, c.Token); n != 0 {
		t.Fatalf("oversized request committed: transcript count = %d", n)
	}
}

// A genuinely malformed body still reports a parse error.
func TestMCPMalformedStillParseError(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no protocol error object in response: %v", body)
	}
	if rc, _ := errObj["code"].(float64); rc != -32700 {
		t.Fatalf("error code = %v, want -32700", errObj["code"])
	}
}
