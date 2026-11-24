package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
)

const (
	maxRoomBytes    = 1 << 20 // decoded message text; empty ending remains possible
	maxLiveRooms    = 1000
	maxLiveBytes    = 64 << 20            // serialized snapshots, including credentials/metadata
	maxRequestBytes = 6*maxMsgSize + 4096 // worst-case JSON escaping plus envelope
)

var errCapacity = errors.New("live-room storage capacity reached")
var budgetMu sync.Mutex
var snapshotBytes = map[string]int{}
var liveBytes int

// budgetMu serializes snapshot reservations through rename and accounting.
// No function holding it takes roomsMu or a room mutex. Room locks come first.
func rememberSnapshot(id string, size int) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	liveBytes += size - snapshotBytes[id]
	snapshotBytes[id] = size
}
func forgetSnapshot(id string) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	liveBytes -= snapshotBytes[id]
	delete(snapshotBytes, id)
}
func privateRoomsDir() error {
	if fi, err := os.Stat(roomsDir); err == nil && fi.Mode().Perm()&0222 == 0 {
		return fmt.Errorf("rooms directory is read-only")
	}
	if err := os.MkdirAll(roomsDir, 0700); err != nil {
		return err
	}
	// MkdirAll alone does not tighten an existing deployment directory.
	return os.Chmod(roomsDir, 0700)
}
func messageBytes(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text)
	}
	return n
}

func badBody(w http.ResponseWriter, err error, detail string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		jsonOut(w, 413, map[string]string{"detail": fmt.Sprintf("request too large: JSON body limit is %d bytes; decoded message text limit is 65536 bytes", maxRequestBytes)})
		return
	}
	jsonOut(w, 400, map[string]string{"detail": detail})
}

// One wrapper for the production mux and tests. Inline script/style are needed
// by the current embedded pages; network connections stay on this origin.
func hardenHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'")
		// Wider than credential JSON alone: also protect room HTML and MCP envelopes.
		if r.URL.Path == "/room" || len(r.URL.Path) >= 6 && r.URL.Path[:6] == "/room/" || r.URL.Path == "/new" || r.URL.Path == "/mcp" {
			w.Header().Set("Cache-Control", "no-store")
		}
		http.MaxBytesHandler(next, maxRequestBytes).ServeHTTP(w, r)
	})
}
