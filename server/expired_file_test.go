package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An expired room touched in the window before the janitor's 30s pass must
// not leave its message file on disk: getRoom's expired path deletes the
// file as well, so "messages are deleted on expiry" holds for the access
// path too, not only on the janitor's cycle.
func TestExpiredRoomAccessDeletesFile(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := createRoom(t, srv.URL)
	uin := c.RoomID
	if _, err := os.Stat(filepath.Join(roomsDir, roomFileName(uin))); err != nil {
		t.Fatalf("room file missing before expiry: %v", err)
	}
	key := strings.ReplaceAll(uin, "-", "")
	past := time.Now().Add(-25 * time.Hour)
	roomsMu.Lock()
	r := rooms[key]
	r.mu.Lock()
	r.created = past
	r.lastMsg = past
	r.joinDeadline = past
	r.mu.Unlock()
	roomsMu.Unlock()
	if got := getRoom(key); got != nil {
		t.Fatal("getRoom returned an expired room")
	}
	if _, err := os.Stat(filepath.Join(roomsDir, roomFileName(uin))); !os.IsNotExist(err) {
		t.Fatalf("expired room's file still on disk after access: err=%v", err)
	}
	if !isTombstoned(key) {
		t.Fatal("expired room not tombstoned on access")
	}
}

// roomFileName mirrors the on-disk naming (UIN without dashes); kept local to
// this test so a server-side rename still fails the test loudly.
func roomFileName(uin string) string {
	return strings.ReplaceAll(uin, "-", "") + ".json"
}
