package main

// Regression: agent-facing MCP/explainer lifetime wording must match the
// corrected contract: guests expire; signed-in rooms do not expire.
import (
	"encoding/json"
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
	if strings.Contains(instr, "ephemeral") || strings.Contains(instr, "permanent") {
		t.Errorf("MCP initialize instructions carry a universal ephemerality claim: %q", instr)
	}
	if !strings.Contains(instr, "does not expire") {
		t.Errorf("MCP initialize instructions lost the permanent-room distinction: %q", instr)
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
	if !strings.Contains(body, "do not expire") {
		t.Errorf("explainer lost the permanent-room distinction")
	}
}

func TestRoomPreviewBriefingAvoidsPrivateLifetimeClaims(t *testing.T) {
	setAuthEnv(t, "")
	srv := newTestServer(t)
	defer srv.Close()
	response, err := http.Post(srv.URL+"/room", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RoomID string `json:"roomId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = http.Get(srv.URL + "/room/" + created.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var preview struct {
		Briefing string `json:"briefing"`
	}
	if err := json.NewDecoder(response.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"private, short-lived", "ephemeral", "permanent room"} {
		if strings.Contains(preview.Briefing, term) {
			t.Errorf("room briefing contains stale claim %q", term)
		}
	}
	if !strings.Contains(preview.Briefing, "The relay can read text") {
		t.Error("room briefing must disclose relay readability")
	}
}
