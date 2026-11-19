package main

// Regression tests for the verify/rejoin attempt counters: they persist
// across an ordinary reload, and the documented best-effort residual (a
// restart landing between the increment and a failed save resets the
// counter) behaves exactly as the code comments describe.

import (
	"fmt"
	"net/http"
	"testing"
)

// reloadRooms simulates a restart: wipe in-memory state, reload from disk.
func reloadRooms() {
	roomsMu.Lock()
	rooms = map[string]*Room{}
	roomsMu.Unlock()
	loadRooms()
}

func verifyCode(t *testing.T, base, uin, token, code string) bool {
	t.Helper()
	status, body := postJSON(t, http.MethodPost, base+"/room/"+uin+"/verify", token, fmt.Sprintf(`{"code":%q}`, code))
	if status != http.StatusOK {
		t.Fatalf("verify: got %d, want 200", status)
	}
	match, _ := body["match"].(bool)
	return match
}

func rejoinSeat(t *testing.T, base, uin, code, seat string) int {
	t.Helper()
	status, _ := postJSON(t, http.MethodPost, base+"/room/"+uin+"/rejoin", "", fmt.Sprintf(`{"code":%q,"seat":%q}`, code, seat))
	return status
}

// Five wrong verifies before a reload must still lock verification after it.
func TestVerifyLockoutSurvivesReload(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < 5; i++ {
		if verifyCode(t, srv.URL, c.RoomID, tokB, "000000") {
			t.Fatalf("attempt %d: wrong code matched", i+1)
		}
	}

	reloadRooms()

	if verifyCode(t, srv.URL, c.RoomID, tokB, c.PairCode) {
		t.Fatal("correct code matched after reload despite 5 persisted failures - lockout did not survive")
	}
	// The room itself is healthy after the reload.
	if code, _ := recvMsg(t, srv.URL, c.RoomID, c.Token); code != http.StatusOK {
		t.Fatalf("recv after reload: got %d, want 200", code)
	}
}

// Rejoin failures accumulate across a reload: 3 before + 2 after reach the
// hourly cap, so the correct code is refused. A reset counter would allow it.
func TestRejoinCounterSurvivesReload(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	joinRoom(t, srv.URL, c.RoomID)
	for i := 0; i < 3; i++ {
		if code := rejoinSeat(t, srv.URL, c.RoomID, "000000", "A"); code != http.StatusNotFound {
			t.Fatalf("wrong rejoin %d: got %d, want 404", i+1, code)
		}
	}

	reloadRooms()

	for i := 0; i < 2; i++ {
		if code := rejoinSeat(t, srv.URL, c.RoomID, "000000", "A"); code != http.StatusNotFound {
			t.Fatalf("wrong rejoin after reload %d: got %d, want 404", i+1, code)
		}
	}
	if code := rejoinSeat(t, srv.URL, c.RoomID, c.PairCode, "A"); code != http.StatusNotFound {
		t.Fatalf("correct rejoin after 5 persisted failures: got %d, want 404 (counter did not survive reload)", code)
	}
}

// Documented residual: with saves failing, a reload resets the in-process
// counter. Until the reload the in-process cap still holds.
func TestVerifyCounterResetAfterFailedSaveAndReload(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := createRoom(t, srv.URL)
	_, tokB := joinRoom(t, srv.URL, c.RoomID)

	withPersistFail(t, "pre")
	for i := 0; i < 5; i++ {
		if verifyCode(t, srv.URL, c.RoomID, tokB, "000000") {
			t.Fatalf("attempt %d: wrong code matched", i+1)
		}
	}
	// In-process cap holds while the process lives, despite failed saves.
	if verifyCode(t, srv.URL, c.RoomID, tokB, c.PairCode) {
		t.Fatal("correct code matched in-process despite 5 failures - in-process cap broken")
	}
	persistFailMode = ""

	reloadRooms()

	// The persisted snapshot kept verify_fails=0: the lockout reset, exactly
	// the accepted residual in the handler comment.
	if !verifyCode(t, srv.URL, c.RoomID, tokB, c.PairCode) {
		t.Fatal("correct code refused after failed-save reload - residual behavior changed")
	}
}
