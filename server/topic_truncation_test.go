package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// createRoomWithTopic creates a room with an explicit topic and returns the
// stored topic as the create response reports it.
func createRoomWithTopic(t *testing.T, base, topic string) struct{ Topic string } {
	t.Helper()
	res, err := http.Post(base+"/room", "application/json", strings.NewReader(`{"topic":`+strconv.Quote(topic)+`}`))
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create room: got %d, want 201", res.StatusCode)
	}
	var c struct{ Topic string }
	if err := json.NewDecoder(res.Body).Decode(&c); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return c
}

// Topic truncation must never corrupt a multi-byte rune. The 200-byte cap is
// a byte cap; a mid-rune cut silently becomes U+FFFD at JSON encoding.
func TestTopicTruncationRuneBoundary(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// 199 ASCII bytes + one 2-byte rune: the naive cut splits the rune.
	crossing := strings.Repeat("x", 199) + "א" // 201 bytes
	c := createRoomWithTopic(t, srv.URL, crossing)
	if got := len(c.Topic); got > 200 {
		t.Fatalf("stored topic %d bytes, want <= 200", got)
	}
	if !utf8.ValidString(c.Topic) {
		t.Fatalf("stored topic is not valid UTF-8: %q", c.Topic[len(c.Topic)-8:])
	}
	if strings.ContainsRune(c.Topic, '�') {
		t.Fatalf("stored topic carries U+FFFD corruption: %q", c.Topic[len(c.Topic)-8:])
	}
	if want := strings.Repeat("x", 199); c.Topic != want {
		t.Fatalf("stored topic = %q..., want the 199 complete bytes", c.Topic[:12])
	}

	// 4-byte rune straddling the boundary: 198 ASCII + emoji (bytes 199-202).
	emoji := strings.Repeat("y", 198) + "🙂" // 202 bytes
	c2 := createRoomWithTopic(t, srv.URL, emoji)
	if !utf8.ValidString(c2.Topic) || strings.ContainsRune(c2.Topic, '�') {
		t.Fatalf("emoji-boundary topic corrupted: %q", c2.Topic[len(c2.Topic)-8:])
	}
	if want := strings.Repeat("y", 198); c2.Topic != want {
		t.Fatalf("emoji-boundary topic = %q..., want the 198 complete bytes", c2.Topic[:12])
	}

	// Pure ASCII over the cap keeps the exact 200-byte prefix (no behavior change).
	ascii := strings.Repeat("z", 250)
	c3 := createRoomWithTopic(t, srv.URL, ascii)
	if want := strings.Repeat("z", 200); c3.Topic != want {
		t.Fatalf("ascii truncation changed: got %d bytes", len(c3.Topic))
	}

	// Under the cap, multibyte content passes through untouched.
	short := "דיון קצר" // 15 bytes
	c4 := createRoomWithTopic(t, srv.URL, short)
	if c4.Topic != short {
		t.Fatalf("short multibyte topic = %q, want %q", c4.Topic, short)
	}
}

// The documented topic contract: the cap is 200 BYTES as stored, inclusive;
// the server stores the topic as received - it does not trim whitespace
// (clients may trim before sending; that is their choice, not the contract).
func TestTopicCapContract(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Exactly 200 bytes is kept whole.
	exact := strings.Repeat("e", 196) + "אא" // 196 + 2*2 = 200 bytes
	if len(exact) != 200 {
		t.Fatalf("test input is %d bytes, want 200", len(exact))
	}
	c := createRoomWithTopic(t, srv.URL, exact)
	if c.Topic != exact {
		t.Fatalf("exactly-200-byte topic = %d bytes, want it stored whole", len(c.Topic))
	}

	// Whitespace-padded topic under the cap is stored verbatim, padding
	// included: the byte cap applies to the topic as received.
	padded := "  " + "דיון" + "  "
	c2 := createRoomWithTopic(t, srv.URL, padded)
	if c2.Topic != padded {
		t.Fatalf("padded topic = %q, want it stored verbatim %q", c2.Topic, padded)
	}
}
