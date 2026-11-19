package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An expired room file that survived deletion (failed reap cleanup) must be
// re-reaped at boot: not loaded, tombstoned, and its deletion retried. This
// is the load-side half of the late-writer residual qualification.
func TestBootReapsExpiredRoomFile(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	key := "AAAAAAAAAAAAA"
	d := roomDisk{
		ID:           key,
		TokenA:       "tokenA-stale",
		Created:      time.Now().Add(-3 * time.Hour),
		JoinDeadline: time.Now().Add(-1 * time.Hour),
		PairCode:     "111111",
	}
	raw, _ := json.Marshal(d)
	if err := os.MkdirAll(roomsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomsDir, key+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loadRooms()
	roomsMu.Lock()
	_, loaded := rooms[key]
	roomsMu.Unlock()
	if loaded {
		t.Fatal("expired room file was loaded as live at boot")
	}
	if !isTombstoned(key) {
		t.Fatal("expired room file not tombstoned at boot")
	}
	if _, err := os.Stat(filepath.Join(roomsDir, key+".json")); !os.IsNotExist(err) {
		t.Fatalf("expired room file not deleted at boot: err=%v", err)
	}
}
