package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reapers (janitor sweep and the getRoom access path) check expiry under
// r.mu; every mutation handler holds r.mu from its reaped precheck through
// its persist. A handler that resolved a room just before expiry must
// serialize with the reap: it either commits before the reap (its
// acknowledged write is then deleted with the room, ordinary expiry
// semantics) or observes reaped at its precheck and rejects without writing,
// so no expired content is persisted after the reap. These tests drive the
// real handlers against real reaps; no timing sleeps.

// forceExpire backdates a resolved room past every deadline under the locks.
func forceExpire(t *testing.T, key string) *Room {
	t.Helper()
	roomsMu.Lock()
	r := rooms[key]
	roomsMu.Unlock()
	if r == nil {
		t.Fatal("room not in map")
	}
	past := time.Now().Add(-25 * time.Hour)
	r.mu.Lock()
	r.created = past
	r.lastMsg = past
	r.joinDeadline = past
	r.mu.Unlock()
	return r
}

// assertReaped checks the common post-reap state for the janitor sweep path.
func assertReaped(t *testing.T, key, uin string, r *Room) {
	t.Helper()
	roomsMu.Lock()
	_, still := rooms[key]
	roomsMu.Unlock()
	if still {
		t.Fatal("swept room still mapped")
	}
	if !isTombstoned(key) {
		t.Fatal("swept room not tombstoned")
	}
	r.mu.Lock()
	reaped := r.reaped
	r.mu.Unlock()
	if !reaped {
		t.Fatal("swept room not marked reaped")
	}
	if _, err := os.Stat(filepath.Join(roomsDir, roomFileName(uin))); !os.IsNotExist(err) {
		t.Fatalf("swept room's file still on disk: err=%v", err)
	}
}

func roomFileExists(uin string) bool {
	_, err := os.Stat(filepath.Join(roomsDir, roomFileName(uin)))
	return err == nil
}

// TestLateWriterAfterJanitorSweep drives every guarded mutation path against
// a room the janitor sweep has already reaped: acknowledged mutations must
// answer expired (410), mint or commit nothing, and leave no file behind;
// best-effort counter writes keep their uniform response and leave no file.
func TestLateWriterAfterJanitorSweep(t *testing.T) {
	cases := []struct {
		name  string
		pair  bool // pair the room before expiring it
		drive func(t *testing.T, r *Room, uin string, c roomCreated) (status int)
		want  int
	}{
		{"send", false, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/msg", strings.NewReader(`{"text":"late write"}`))
			rr := httptest.NewRecorder()
			handleSendAs(rr, req, r, "A", false)
			return rr.Code
		}, 410},
		{"join", false, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/join", nil)
			rr := httptest.NewRecorder()
			handleJoin(rr, req, r)
			return rr.Code
		}, 410},
		{"rejoin rotation", true, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/rejoin", strings.NewReader(`{"code":"`+c.PairCode+`","seat":"A"}`))
			rr := httptest.NewRecorder()
			handleRejoin(rr, req, r)
			return rr.Code
		}, 410},
		{"extend", false, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/extend", strings.NewReader(`{"hours":2}`))
			req.Header.Set("Authorization", "Bearer "+c.Token)
			rr := httptest.NewRecorder()
			handleExtend(rr, req, r)
			return rr.Code
		}, 410},
		{"rejoin counter write", true, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/rejoin", strings.NewReader(`{"code":"wrong-code","seat":"A"}`))
			rr := httptest.NewRecorder()
			handleRejoin(rr, req, r)
			return rr.Code // uniform not-found: no oracle, no file
		}, 404},
		{"verify counter write", true, func(t *testing.T, r *Room, uin string, c roomCreated) int {
			req := httptest.NewRequest("POST", "/room/"+uin+"/verify", strings.NewReader(`{"code":"wrong-code"}`))
			req.Header.Set("Authorization", "Bearer "+c.Token)
			rr := httptest.NewRecorder()
			handleVerify(rr, req, r)
			return rr.Code // unchanged 200 match:false, no file
		}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.Close()
			c := createRoom(t, srv.URL)
			uin := c.RoomID
			key := strings.ReplaceAll(uin, "-", "")
			if tc.pair {
				if status, _ := joinRoom(t, srv.URL, uin); status != http.StatusOK && status != http.StatusCreated {
					t.Fatalf("pair join: got %d", status)
				}
			}
			r := forceExpire(t, key)
			sweepExpired(time.Now())
			assertReaped(t, key, uin, r)

			if got := tc.drive(t, r, uin, c); got != tc.want {
				t.Fatalf("late %s answered %d, want %d", tc.name, got, tc.want)
			}
			if roomFileExists(uin) {
				t.Fatalf("late %s left a room file on disk", tc.name)
			}
		})
	}
}

// TestLateWriterAfterReapLeavesNoFile covers the access-path reap (getRoom)
// for the send handler: same serialization, reached without the janitor.
func TestLateWriterAfterReapLeavesNoFile(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")
	r := forceExpire(t, key)
	if got := getRoom(key); got != nil {
		t.Fatal("getRoom returned an expired room")
	}

	req := httptest.NewRequest("POST", "/room/"+uin+"/msg", strings.NewReader(`{"text":"late write"}`))
	rr := httptest.NewRecorder()
	handleSendAs(rr, req, r, "A", false)
	if rr.Code != 410 {
		t.Fatalf("late send answered %d, want 410", rr.Code)
	}
	r.mu.Lock()
	n := len(r.msgs)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("late send committed %d in-memory messages, want 0", n)
	}
	if roomFileExists(uin) {
		t.Fatal("late send left a room file on disk")
	}
}

// TestReapDeleteFailureLeavesResidue forces the cleanup delete itself to fail
// (via the osRemove seam, deterministic under any test identity) and records
// the honest outcome: the room is still unmapped, tombstoned and
// unacknowledgeable (410), but its file remains as residue. Nothing here or
// at restart guarantees later removal, so no residue-lifetime bound is
// promised. The late writer rejects at its precheck without writing, so the
// residue stays the pre-reap content and no further delete is attempted.
func TestReapDeleteFailureLeavesResidue(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	key := strings.ReplaceAll(uin, "-", "")

	removeCalls := 0
	wantErr := errors.New("injected delete failure")
	realRemove := osRemove
	osRemove = func(path string) error {
		if strings.Contains(path, key) {
			removeCalls++
			return wantErr
		}
		return realRemove(path)
	}
	t.Cleanup(func() { osRemove = realRemove })

	// Fixture proof: under the stub, deleting this room's file really fails.
	if err := deleteRoomFile(key); !errors.Is(err, wantErr) {
		t.Fatalf("failure fixture did not fail: err=%v", err)
	}
	if !roomFileExists(uin) {
		t.Fatal("failure fixture removed the file it was meant to retain")
	}

	r := forceExpire(t, key)
	sweepExpired(time.Now())
	roomsMu.Lock()
	_, still := rooms[key]
	roomsMu.Unlock()
	if still {
		t.Fatal("swept room still mapped despite delete failure")
	}
	if !isTombstoned(key) {
		t.Fatal("swept room not tombstoned despite delete failure")
	}

	// A late writer still gets no acknowledgment; it rejects at its
	// precheck without writing and attempts no further delete. The
	// residue remains, logged, with no promised bound.
	req := httptest.NewRequest("POST", "/room/"+uin+"/msg", strings.NewReader(`{"text":"late write"}`))
	rr := httptest.NewRecorder()
	handleSendAs(rr, req, r, "A", false)
	if rr.Code != 410 {
		t.Fatalf("late send answered %d, want 410", rr.Code)
	}
	if !roomFileExists(uin) {
		t.Fatal("residue file unexpectedly gone")
	}
	if removeCalls != 2 {
		t.Fatalf("expected exactly 2 failed delete attempts (fixture, sweep); the precheck must not write or re-delete, got %d", removeCalls)
	}
}
