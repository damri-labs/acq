package main

import (
	"net/http"
	"testing"
)

// Qualification of the capped-seat-cannot-end lifecycle limitation.
//
// The unanswered cap (50 consecutive messages from one seat, 429) exists so a
// one-sided two-model loop terminates on its own (SPEC: "The unanswered-
// message cap is what makes a two-model loop terminate on its own rather than
// run until the room expires."). Because an end travels the seat send path,
// the cap also blocks {text:'', end:true} from the capped seat: the one
// participant motivated to close a room whose peer never answers is the one
// that cannot. Guest rooms linger to expiry; for permanent rooms, closure
// absent a responsive peer depends on routes outside this path (retention and
// deletion behavior are out of scope for this change and not exhaustively
// verified here).
//
// The cap is per-seat consecutive, not room-wide: the peer's count is
// independent, so an unresponsive-but-present peer can still end (proven
// below). It is also a distinct layer from the 200-message room total
// (uniform 404) and the per-IP rate limits (create 10/h, failed lookups,
// probe penalties).

// Control, true before and after the proposed correction: the cap does not
// stop the UNCAPPED peer seat from ending the room.
func TestUnansweredCapPeerCanEnd(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < maxUnanswered; i++ {
		if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "m", false); got != http.StatusOK {
			t.Fatalf("seed %d: got %d", i, got)
		}
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "one more", false); got != http.StatusTooManyRequests {
		t.Fatalf("ordinary send past cap: got %d, want 429", got)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, tokB, "", true); got != http.StatusOK {
		t.Fatalf("peer end at A's cap: got %d, want 200 (cap is per-seat, not room-wide)", got)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "too late", false); got != http.StatusGone {
		t.Fatalf("send after end: got %d, want 410", got)
	}
}

// The limitation, and the proposed minimal correction: exempt an empty-text
// end from the unanswered cap. Text-bearing sends stay capped, including an
// end that carries a final message, so the correction grants no extra
// ordinary-message channel; the one appended message is the terminal end
// marker itself.
func TestCappedSeatEmptyEnd(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < maxUnanswered; i++ {
		if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "m", false); got != http.StatusOK {
			t.Fatalf("seed %d: got %d", i, got)
		}
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "one more", false); got != http.StatusTooManyRequests {
		t.Fatalf("ordinary send past cap: got %d, want 429", got)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "last word", true); got != http.StatusTooManyRequests {
		t.Fatalf("text-bearing end past cap: got %d, want 429 (no extra ordinary messages)", got)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "", true); got != http.StatusOK {
		t.Fatalf("empty end at cap: got %d, want 200", got)
	}
	// Read with the PEER token: a seat's delivery view excludes its own messages.
	code, msgs := recvMsg(t, srv.URL, c.RoomID, tokB)
	if code != http.StatusOK {
		t.Fatalf("read after end: got %d", code)
	}
	ends := 0
	for _, m := range msgs {
		if m.End {
			ends++
			if m.From != "A" || m.Text != "" {
				t.Errorf("end message: got from=%s text=%q, want from=A empty", m.From, m.Text)
			}
		}
	}
	if ends != 1 {
		t.Errorf("end messages on wire: got %d, want exactly 1", ends)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "too late", false); got != http.StatusGone {
		t.Fatalf("send after own end: got %d, want 410", got)
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "", true); got != http.StatusGone {
		t.Fatalf("second end: got %d, want 410 (end is terminal, not repeatable)", got)
	}
}

// Boundary the proposal deliberately does NOT change: the 200-message room
// total is a hard storage bound (uniform 404), and an end at exactly 200
// messages is still rejected. Flagged as a separate, rarer lifecycle edge.
func TestFullRoomEndBoundary(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < maxMsgsPerRoom; i++ {
		tok := c.Token
		if i%2 == 1 {
			tok = tokB
		}
		if got := sendMsg(t, srv.URL, c.RoomID, tok, "m", false); got != http.StatusOK {
			t.Fatalf("fill %d: got %d", i, got)
		}
	}
	if got := sendMsg(t, srv.URL, c.RoomID, c.Token, "", true); got != http.StatusNotFound {
		t.Fatalf("end at room total: got %d, want 404 (total bound unchanged by proposal)", got)
	}
}
