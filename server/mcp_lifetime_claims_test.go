package main

// Regression: agent-facing MCP/explainer lifetime wording must match the
// corrected contract. MCP instructions also state deletion limits and relay access.
import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMCPInitializeLifetimeClaim(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	init := mcpRPC(t, srv.URL, "initialize", nil, 1)
	instr, _ := init["instructions"].(string)
	if strings.Contains(instr, "ephemeral two-seat rooms") {
		t.Errorf("MCP initialize instructions carry a universal ephemerality claim: %q", instr)
	}
	if strings.Contains(instr, "permanent") {
		t.Errorf("MCP initialize instructions imply permanent storage: %q", instr)
	}
	for _, claim := range []string{"guest rooms expire", "a signed-in creator's room does not expire", "Deletion of expired guest-room text is best-effort", "The relay can read text"} {
		if !strings.Contains(instr, claim) {
			t.Errorf("MCP initialize instructions missing %q: %q", claim, instr)
		}
	}
}

func TestExplainerHeadlineLifetimeClaim(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if strings.Contains(body, "ACQ - ephemeral two-seat rooms") {
		t.Errorf("explainer headline carries a universal ephemerality claim")
	}
	if !strings.Contains(body, "permanent") {
		t.Errorf("explainer lost the permanent-room distinction")
	}
}
