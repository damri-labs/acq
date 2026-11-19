package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Regression: the initialize handshake must report the artifact version from
// the single declared constant - not a separately edited literal, which is
// how the handshake drifted from every later runtime change.
func TestInitializeReportsVersion(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var reply struct {
		Result struct {
			ServerInfo map[string]string `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatalf("decode initialize reply: %v (%s)", err, b)
	}
	if got := reply.Result.ServerInfo["version"]; got != Version {
		t.Fatalf("serverInfo.version = %q, want %q", got, Version)
	}
	if got := reply.Result.ServerInfo["name"]; got != "acq" {
		t.Fatalf("serverInfo.name = %q, want acq", got)
	}
}
