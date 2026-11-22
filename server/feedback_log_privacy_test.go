package main

// Instinct on behalf of Tal Chaim Damri: feedback remains in its file, never raw app logs.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackLogsContentLengthsOnly(t *testing.T) {
	for _, roomScoped := range []bool{false, true} {
		for _, brokenFile := range []bool{false, true} {
			name := "public"
			if roomScoped {
				name = "room"
			}
			if brokenFile {
				name += "/file_unavailable"
			} else {
				name += "/file_written"
			}
			t.Run(name, func(t *testing.T) {
				srv := newTestServer(t)
				defer srv.Close()
				path := "/feedback"
				token := ""
				if roomScoped {
					c := createRoom(t, srv.URL)
					path = "/room/" + c.RoomID + "/feedback"
					token = c.Token
				}
				if brokenFile {
					feedbackPath = filepath.Join(t.TempDir(), "missing-parent", "feedback.log")
				}
				text := "FICTIONAL_FEEDBACK_TEXT_שלום\nsecond line"
				purpose := "FICTIONAL_PURPOSE_בדיקה"
				payload, _ := json.Marshal(map[string]string{"text": text, "purpose": purpose})
				var buf bytes.Buffer
				old := log.Writer()
				log.SetOutput(&buf)
				defer log.SetOutput(old)
				req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if (!brokenFile && res.StatusCode != 200) || (brokenFile && res.StatusCode != 500) {
					t.Fatalf("unexpected status %d", res.StatusCode)
				}
				var result map[string]any
				if err := json.Unmarshal(b, &result); err != nil {
					t.Fatal(err)
				}
				if brokenFile && result["stored"] != false {
					t.Fatalf("expected definite failure, got %+v", result)
				}
				logs := buf.String()
				for _, fragment := range []string{"FICTIONAL_FEEDBACK_TEXT", "FICTIONAL_PURPOSE", "שלום", "בדיקה", "second line", token} {
					if fragment != "" && strings.Contains(logs, fragment) {
						t.Fatal("content or token leaked in app logs")
					}
				}
				expected := "feedback_public purpose_bytes="
				if roomScoped {
					expected = "feedback id="
				}
				if !strings.Contains(logs, expected) || !strings.Contains(logs, fmt.Sprintf("purpose_bytes=%d text_bytes=%d", len(purpose), len(text))) {
					t.Fatal("missing noncontent diagnostic")
				}
				if brokenFile {
					if !strings.Contains(logs, "feedback_file_error") {
						t.Fatal("file error diagnostic lost")
					}
					return
				}
				saved, err := os.ReadFile(feedbackPath)
				if err != nil {
					t.Fatal(err)
				}
				var entry map[string]any
				if err = json.Unmarshal(saved, &entry); err != nil {
					t.Fatal(err)
				}
				if entry["text"] != text || entry["purpose"] != purpose {
					t.Fatal("file content preservation changed")
				}
			})
		}
	}
}
