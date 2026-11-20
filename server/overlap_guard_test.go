package main

// Overlap-guard regression tests. Confirmed defect (two processes briefly
// serving one rooms directory during a rolling deploy): the second
// process's whole-snapshot save durably clobbers messages the first
// process accepted, and can assign duplicate seqs from its stale memory.
// The send path now re-reads a room file newer than this process's last
// load or write before building its candidate, and treats a vanished file
// as an expired room.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeForeignSnapshot simulates the overlapping process committing its own
// whole-room snapshot to the shared rooms directory, with an mtime
// guaranteed newer than this server's last write.
func writeForeignSnapshot(t *testing.T, roomID string, mutate func(*roomDisk)) {
	t.Helper()
	p := filepath.Join(roomsDir, roomFileName(roomID))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("foreign read: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("foreign unmarshal: %v", err)
	}
	mutate(&d)
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("foreign marshal: %v", err)
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatalf("foreign write: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("foreign chtimes: %v", err)
	}
}

func TestSendRebasesNewerForeignSnapshot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false); st != http.StatusOK {
		t.Fatalf("send one: got %d, want 200", st)
	}

	// The overlapping process accepts a message and saves its snapshot.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.Msgs = append(d.Msgs, Msg{Seq: 2, From: "B", Ts: float64(time.Now().UnixMilli()) / 1000, Text: "foreign-two"})
	})

	// This process's next send must rebase onto the foreign snapshot: the
	// new message takes seq 3 and the foreign message survives on disk.
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"after-overlap"}`)
	if st != http.StatusOK {
		t.Fatalf("send after overlap: got %d %v, want 200", st, body)
	}
	if body["seq"] != float64(3) {
		t.Fatalf("seq after overlap: got %v, want 3 (a stale-memory save would assign a duplicate seq 2)", body["seq"])
	}

	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	if len(d.Msgs) != 3 {
		t.Fatalf("disk msgs: got %d, want 3 (foreign message clobbered by whole-snapshot save)", len(d.Msgs))
	}
	seqs := map[int]bool{}
	for _, m := range d.Msgs {
		if seqs[m.Seq] {
			t.Fatalf("duplicate seq %d on disk", m.Seq)
		}
		seqs[m.Seq] = true
	}
	if d.Msgs[1].Text != "foreign-two" || d.Msgs[2].Text != "after-overlap" {
		t.Fatalf("disk msgs: %+v", d.Msgs)
	}

	// B's read returns what B has not been delivered: both A messages
	// (B's own foreign message is not delivered back to B).
	_, msgs := recvMsg(t, srv.URL, c.RoomID, tokB)
	if len(msgs) != 2 || msgs[0].Text != "one" || msgs[1].Text != "after-overlap" {
		t.Fatalf("recv after rebase: got %+v, want [one after-overlap]", msgs)
	}
}

func TestSendOnVanishedRoomFileReturnsGone(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if _, tokB := joinRoom(t, srv.URL, c.RoomID); tokB == "" {
		t.Fatal("join: no token")
	}
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false); st != http.StatusOK {
		t.Fatalf("send one: got %d, want 200", st)
	}

	// The overlapping process reaped and deleted the room (expiry or owner
	// deletion): the file vanishes under this process's stale memory.
	if err := os.Remove(filepath.Join(roomsDir, roomFileName(c.RoomID))); err != nil {
		t.Fatalf("remove room file: %v", err)
	}

	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"into the void"}`)
	if st != http.StatusGone {
		t.Fatalf("send on vanished file: got %d, want 410 (a stale-memory save would silently recreate the room without the deleted history)", st)
	}

	// The room is unmapped and tombstoned: later reads answer expired, not
	// a phantom room resurrected from stale memory.
	if st, _ := recvMsg(t, srv.URL, c.RoomID, c.Token); st != http.StatusGone {
		t.Fatalf("recv after vanish: got %d, want 410", st)
	}
}

// Lock-order regression: the documented order is roomsMu -> r.mu
// (getRoom, sweepExpired). The vanished-file path of the guard runs under
// r.mu (the send path holds it) and must not take roomsMu; an earlier
// version did, and a concurrent sweep or lookup wedged the server.
func TestOverlapGuardVanishedPathTakesNoRoomsMu(t *testing.T) {
	dir := t.TempDir()
	old := roomsDir
	roomsDir = dir
	defer func() { roomsDir = old }()
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	tombMu.Lock()
	tombstones = map[string]time.Time{}
	tombMu.Unlock()

	r := &Room{id: "probeRoom0001", tokenA: "tokA", notify: make(chan struct{}), joinDeadline: time.Now().Add(time.Hour)}
	if outcome, err := writeRoom(roomDiskFrom(r)); err != nil || outcome == persistFailedDefinite {
		t.Fatalf("setup writeRoom: %v %v", outcome, err)
	}
	roomsMu.Lock()
	rooms[r.id] = r
	roomsMu.Unlock()
	defer func() { roomsMu.Lock(); delete(rooms, r.id); roomsMu.Unlock() }()

	// Foreign reap: the file is gone while this process still maps the room.
	if err := os.Remove(filepath.Join(dir, r.id+".json")); err != nil {
		t.Fatalf("setup remove: %v", err)
	}

	done := make(chan bool, 1)
	roomsMu.Lock() // held as sweepExpired/getRoom hold it while nesting into r.mu
	go func() {
		r.mu.Lock()
		gone := refreshRoomFromDiskLocked(r) // send-path lock state: r.mu held
		r.mu.Unlock()
		done <- gone
	}()
	var gone bool
	select {
	case gone = <-done:
		// guard completed while the test holds roomsMu: no inversion.
	case <-time.After(2 * time.Second):
		// Release so the blocked goroutine can finish, then fail.
		roomsMu.Unlock()
		t.Fatalf("guard blocked on roomsMu while holding r.mu: AB-BA lock inversion")
	}
	roomsMu.Unlock()
	if !gone {
		t.Fatalf("guard: gone=false, want true")
	}

	if !r.reaped || !isTombstoned(r.id) {
		t.Fatalf("guard: reaped=%v tombstoned=%v, want both true", r.reaped, isTombstoned(r.id))
	}
	// The guard must not unmap under r.mu: the room is still mapped, and
	// sweepExpired collects it without touching the renewed notify channel
	// (a double close would panic).
	roomsMu.Lock()
	_, mapped := rooms[r.id]
	roomsMu.Unlock()
	if !mapped {
		t.Fatal("guard unmapped the room from under r.mu; unmapping belongs to getRoom/sweepExpired")
	}
	sweepExpired(time.Now())
	roomsMu.Lock()
	_, mapped = rooms[r.id]
	roomsMu.Unlock()
	if mapped {
		t.Fatal("sweepExpired did not collect the reaped room")
	}
	// A later expiry reap via getRoom must not double-close either.
	if got := getRoom(r.id); got != nil {
		t.Fatalf("getRoom returned a reaped room: %+v", got)
	}
}

// A parked long-poll reader must be woken by the vanished-file guard and
// answer 410 promptly, not sleep out its wait.
func TestVanishedRoomWakesParkedReaders(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if tokB == "" {
		t.Fatal("join: no token")
	}

	recvDone := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/msg?wait=30", nil)
		req.Header.Set("Authorization", "Bearer "+tokB)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			recvDone <- -1
			return
		}
		res.Body.Close()
		recvDone <- res.StatusCode
	}()
	// Let the reader park before the vanish.
	time.Sleep(300 * time.Millisecond)

	if err := os.Remove(filepath.Join(roomsDir, roomFileName(c.RoomID))); err != nil {
		t.Fatalf("remove room file: %v", err)
	}
	// The next send trips the guard: 410 for the sender, and the parked
	// reader is woken to the same answer.
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"late"}`)
	if st != http.StatusGone {
		t.Fatalf("send on vanished file: got %d, want 410", st)
	}
	select {
	case code := <-recvDone:
		if code != http.StatusGone {
			t.Fatalf("parked recv: got %d, want 410", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked recv not woken by the vanished-file guard")
	}
}

// Stream (SSE) counterpart of TestVanishedRoomWakesParkedReaders: a parked
// event stream on a foreign-deleted room must be woken by the guard and
// end with the expired event promptly (heartbeat is 25s away, so a quick
// end can only come from the guard's close). Negative control: against
// the pre-remediation head (no reaped check in handleStream, no wake in
// the vanished path) the stream never ends and this test times out.
func TestVanishedRoomWakesParkedStream(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if tokB == "" {
		t.Fatal("join: no token")
	}

	type streamResult struct {
		body string
		err  error
	}
	streamDone := make(chan streamResult, 1)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/room/"+c.RoomID+"/stream", nil)
		req.Header.Set("Authorization", "Bearer "+tokB)
		res, err := client.Do(req)
		if err != nil {
			streamDone <- streamResult{err: err}
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		streamDone <- streamResult{body: string(b), err: err}
	}()
	// Let the stream connect and park before the vanish.
	time.Sleep(300 * time.Millisecond)

	if err := os.Remove(filepath.Join(roomsDir, roomFileName(c.RoomID))); err != nil {
		t.Fatalf("remove room file: %v", err)
	}
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"late"}`)
	if st != http.StatusGone {
		t.Fatalf("send on vanished file: got %d, want 410", st)
	}
	select {
	case r := <-streamDone:
		if r.err != nil {
			t.Fatalf("stream read: %v", r.err)
		}
		if !strings.Contains(r.body, "event: end") || !strings.Contains(r.body, `"reason":"expired"`) {
			t.Fatalf("stream ended without the expired end event: %q", r.body)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("parked stream not woken by the vanished-file guard")
	}
}

// The non-send mutation paths (join, extend, verify, rejoin) write whole
// room snapshots too: on a stale in-memory copy they would clobber foreign
// messages, double-mint seats, or resurrect deleted rooms exactly like the
// send path did. These tests pin the guard's coverage of those handlers.

func TestJoinRebasesNewerForeignSnapshot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	// The overlapping process pairs the room first.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.PairedAt = time.Now()
		d.TokenB = "foreign-tok"
		d.RecoveryB = "foreign-rec"
	})

	// This process's stale memory still shows the room unclaimed: without
	// the rebase it would mint a second seat B and overwrite the foreign
	// joiner's credential on disk.
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/join", "", "{}")
	if st != http.StatusConflict {
		t.Fatalf("join after foreign pairing: got %d, want 409 (stale memory would double-mint seat B)", st)
	}
	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	if d.TokenB != "foreign-tok" {
		t.Fatalf("foreign joiner's credential overwritten: tokenB=%q", d.TokenB)
	}
}

func TestExtendPreservesForeignMessages(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if _, tokB := joinRoom(t, srv.URL, c.RoomID); tokB == "" {
		t.Fatal("join: no token")
	}
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false); st != http.StatusOK {
		t.Fatalf("send one: got %d, want 200", st)
	}

	// The overlapping process accepts a message this process has not seen.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.Msgs = append(d.Msgs, Msg{Seq: 2, From: "B", Ts: float64(time.Now().UnixMilli()) / 1000, Text: "foreign-two"})
	})

	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/extend", c.Token, `{"hours":2}`)
	if st != http.StatusOK {
		t.Fatalf("extend: got %d, want 200", st)
	}
	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	if len(d.Msgs) != 2 || d.Msgs[1].Text != "foreign-two" {
		t.Fatalf("extend clobbered the foreign message: %+v", d.Msgs)
	}
}

func TestVerifyCountsForeignFailures(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if _, tokB := joinRoom(t, srv.URL, c.RoomID); tokB == "" {
		t.Fatal("join: no token")
	}

	// The overlapping process has already counted four failed verifies.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.VerifyFails = 4
	})

	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/verify", c.Token, `{"code":"000000"}`)
	if st != http.StatusOK || body["match"] != false {
		t.Fatalf("verify: got %d %v, want 200 match=false", st, body)
	}
	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	if d.VerifyFails != 5 {
		t.Fatalf("verify counter: got %d, want 5 (stale memory would reset the foreign count to 1)", d.VerifyFails)
	}
}

func TestRejoinUsesForeignRotatedSecret(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if _, tokB := joinRoom(t, srv.URL, c.RoomID); tokB == "" {
		t.Fatal("join: no token")
	}

	// The overlapping process already rotated seat B's credential.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.TokenB = "rotated-tok"
		d.RecoveryB = "fresh-rec"
		d.RekeyedB = time.Now()
	})

	// The fresh recovery code works against the rebased state; stale
	// memory would 404 it.
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/rejoin", "", `{"code":"fresh-rec","seat":"B"}`)
	if st != http.StatusOK {
		t.Fatalf("rejoin with fresh recovery code: got %d %v, want 200", st, body)
	}
	if body["token"] == "" || body["token"] == "rotated-tok" {
		t.Fatalf("rejoin did not rotate: %v", body)
	}
}

func TestJoinOnVanishedRoomFileReturnsGone(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if err := os.Remove(filepath.Join(roomsDir, roomFileName(c.RoomID))); err != nil {
		t.Fatalf("remove room file: %v", err)
	}
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/join", "", "{}")
	if st != http.StatusGone {
		t.Fatalf("join on vanished file: got %d, want 410 (stale memory would resurrect the room)", st)
	}
}

func TestExtendOnVanishedRoomFileReturnsGone(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	if _, tokB := joinRoom(t, srv.URL, c.RoomID); tokB == "" {
		t.Fatal("join: no token")
	}
	if err := os.Remove(filepath.Join(roomsDir, roomFileName(c.RoomID))); err != nil {
		t.Fatalf("remove room file: %v", err)
	}
	st, _ := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/extend", c.Token, `{"hours":2}`)
	if st != http.StatusGone {
		t.Fatalf("extend on vanished file: got %d, want 410 (stale memory would resurrect the room)", st)
	}
}

func TestSendAfterOwnOtherMutationNoFalseRebase(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false); st != http.StatusOK {
		t.Fatalf("send one: got %d, want 200", st)
	}
	// Ordinary single-writer operation: consecutive sends keep sequential
	// seqs with no reload side effects.
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", tokB, `{"text":"two"}`)
	if st != http.StatusOK || body["seq"] != float64(2) {
		t.Fatalf("second send: got %d %v, want 200 seq=2", st, body)
	}
	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	if len(d.Msgs) != 2 || d.Msgs[0].Text != "one" || d.Msgs[1].Text != "two" {
		t.Fatalf("disk msgs: %+v", d.Msgs)
	}
}

// writeRawForeignFile places arbitrary bytes at the room's path with an
// mtime guaranteed newer than this server's last write. It simulates the
// defensive case the overlap guard must refuse: a snapshot this process
// cannot use - corrupt bytes, or a validly encoded snapshot that is not
// this room (a misplaced or foreign-format file).
func writeRawForeignFile(t *testing.T, roomID string, payload []byte) {
	t.Helper()
	p := filepath.Join(roomsDir, roomFileName(roomID))
	if err := os.WriteFile(p, payload, 0o600); err != nil {
		t.Fatalf("raw foreign write: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("raw foreign chtimes: %v", err)
	}
}

// An unparseable newer snapshot must not be admitted: the mutation runs
// from the last valid in-memory state (no message or credential loss),
// the room stays alive, and the guard keeps working afterwards. The
// mutation's own write then restores a valid snapshot at the path -
// under the single-writer deployment contract an unreadable file means
// corruption, so restoring the last valid state is the repair; a
// concurrent writer of a different format is outside that contract.
func TestSendRefusesUnreadableForeignSnapshot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	if st := sendMsg(t, srv.URL, c.RoomID, c.Token, "one", false); st != http.StatusOK {
		t.Fatalf("send one: got %d, want 200", st)
	}

	// Corruption lands at the room's path, newer than our last write.
	writeRawForeignFile(t, c.RoomID, []byte(`{"id":`))

	// The send proceeds from the last valid state: seq 2, not a reset,
	// and neither seat's credential is disturbed by the garbage.
	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"after-corruption"}`)
	if st != http.StatusOK {
		t.Fatalf("send after corruption: got %d %v, want 200 (garbage must not be adopted)", st, body)
	}
	if body["seq"] != float64(2) {
		t.Fatalf("seq after corruption: got %v, want 2 (adopting the garbage would have zeroed the history)", body["seq"])
	}
	if st, _ := recvMsg(t, srv.URL, c.RoomID, tokB); st != http.StatusOK {
		t.Fatalf("recv as seat B after corruption: got %d, want 200 (credentials must survive)", st)
	}

	// The send's own write restored a valid snapshot holding both messages.
	b, err := os.ReadFile(filepath.Join(roomsDir, roomFileName(c.RoomID)))
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("room file still corrupt after send: %v", err)
	}
	if want := strings.ReplaceAll(c.RoomID, "-", ""); d.ID != want || len(d.Msgs) != 2 {
		t.Fatalf("restored snapshot: id=%q msgs=%d, want id %q and 2 messages", d.ID, len(d.Msgs), want)
	}

	// The guard did not latch onto the garbage: a later valid foreign
	// snapshot is still rebased onto normally.
	writeForeignSnapshot(t, c.RoomID, func(d *roomDisk) {
		d.Msgs = append(d.Msgs, Msg{Seq: 3, From: "B", Ts: float64(time.Now().UnixMilli()) / 1000, Text: "foreign-three"})
	})
	st, body = postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/msg", c.Token, `{"text":"after-recovery"}`)
	if st != http.StatusOK || body["seq"] != float64(4) {
		t.Fatalf("send after valid foreign write: got %d seq=%v, want 200 seq=4 (guard must recover after refusing garbage)", st, body["seq"])
	}
}

// A newer snapshot that parses but is not this room - a misplaced or
// foreign-format file - must not be admitted either, even when it plants
// credentials: join keeps operating on the last valid state and mints a
// fresh seat B instead of adopting the planted ones.
func TestJoinRefusesForeignIdentitySnapshot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)

	p := filepath.Join(roomsDir, roomFileName(c.RoomID))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read room file: %v", err)
	}
	var foreign roomDisk
	if err := json.Unmarshal(b, &foreign); err != nil {
		t.Fatalf("unmarshal room file: %v", err)
	}
	foreign.ID = "ZZZZZZ"
	foreign.PairedAt = time.Now()
	foreign.TokenB = "planted-foreign-tok"
	foreign.RecoveryB = "planted-foreign-rec"
	payload, err := json.Marshal(foreign)
	if err != nil {
		t.Fatalf("marshal foreign snapshot: %v", err)
	}
	writeRawForeignFile(t, c.RoomID, payload)

	st, body := postJSON(t, http.MethodPost, srv.URL+"/room/"+c.RoomID+"/join", "", "{}")
	if st != http.StatusOK {
		t.Fatalf("join over foreign-identity snapshot: got %d %v, want 200 (the planted snapshot must not be adopted)", st, body)
	}
	if body["token"] == "" || body["token"] == "planted-foreign-tok" {
		t.Fatalf("join token: got %v, want a freshly minted token, not the planted one", body["token"])
	}
	if body["recoveryCode"] == "" || body["recoveryCode"] == "planted-foreign-rec" {
		t.Fatalf("join recoveryCode: got %v, want a freshly minted code, not the planted one", body["recoveryCode"])
	}

	b, err = os.ReadFile(p)
	if err != nil {
		t.Fatalf("read room file after join: %v", err)
	}
	var d roomDisk
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal room file after join: %v", err)
	}
	if want := strings.ReplaceAll(c.RoomID, "-", ""); d.ID != want {
		t.Fatalf("room file id: got %q, want %q", d.ID, want)
	}
	if d.TokenB != body["token"] || d.RecoveryB != body["recoveryCode"] {
		t.Fatalf("persisted credentials do not match the join response (planted foreign values must be gone)")
	}
}
