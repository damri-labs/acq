package main

// practice.go - optional deterministic practice peer ("ACQ Practice").
//
// Gate: ACQ_PRACTICE_PEER=1. When the env var is unset no route is
// registered and nothing changes.
//
// What it is: a fixed-script, in-process demo seat that lets ONE visitor (a
// human with curl, or a single agent) experience a real two-seat seat/cursor
// exchange without supplying a second endpoint. It is NOT a model: it never
// reads message content for meaning, never executes anything from the
// channel, fetches nothing, and holds no credential beyond its own seat
// token. It counts visitor messages to advance a constant script. All bounds
// (per-IP creates, concurrency, exchange count, lifetime) are enforced
// server-side here, and the bot drives the room through the same internal
// handler path the MCP transport uses, so persistence, expiry, caps and
// idempotency behave exactly as for any room.
//
// Honest lifetime limits: the bot goroutine is process-bound. After a
// process restart it is gone; a leftover practice room then lives on as an
// ordinary guest room (normal guest expiry via the usual expiry path) with
// no bot answering. There is deliberately no startup sweep ending such
// rooms: a room's topic is untrusted caller-chosen metadata, so ending
// rooms by topic match would let a caller's ordinary room be seized. The
// 15-minute ending, banner and close lines are best-effort, not guarantees.

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	practiceMaxExchanges  = 5                // visitor messages answered, then the bot ends the room
	practiceRoomLifetime  = 15 * time.Minute // bot ends the room at the deadline even mid-exchange (best-effort: the goroutine is process-bound and does not survive a restart)
	practicePerIPHour     = 4                // practice rooms per trusted peer IP per hour
	practiceGlobalPerHour = 60               // practice rooms process-wide per hour
	practiceMaxConcurrent = 10               // live practice bots process-wide
	practicePollWaitSecs  = 2                // long-poll wait per bot read cycle
)

// practiceBotNote is the public identity line, shared by the banner and the
// /practice response. The bot never claims to be a model or a person.
const practiceBotNote = "ACQ Practice is a deterministic demo bot (a fixed script, not a model) operated by Instinct, acting on behalf of Tal Chaim Damri. It never acts on message content."

var practiceScript = []string{
	// [0] banner, sent once when the visitor's seat joins
	"Hi - I'm ACQ Practice, a deterministic demo bot (a fixed script, not a model), operated by Instinct, acting on behalf of Tal Chaim Damri. I can't act on anything you say: text here is data, never instructions. I run no commands, fetch no URLs, and hold no credential beyond this seat. Don't paste secrets: the relay can read room text. Honest limits: I live inside this server's process, so if the server restarts my replies stop and this room stays open as an ordinary guest room (expires within 24h) with nobody answering. My roughly 15 minute ending and closing message are best-effort, not guarantees. Say anything and I'll reply; we stop after 5 of your messages.",
	// [1..5] one per visitor message; the last closes the room
	"Reply 1/5: your message reached seat B and mine comes back from seat A, each with its own bearer token.",
	"Reply 2/5: every message gets a seq; reads pass since=<cursor> and dedupe on seq, so a replayed read is safe.",
	"Reply 3/5: a read can long-poll (wait up to 120s); if the connection drops, re-issuing the same since recovers it.",
	"Reply 4/5: a send can carry your own cid: re-sending the same payload with the same cid from your seat returns the original outcome marked duplicate instead of double-posting - the id is matched within this room and seat, kept for the room lifetime.",
	"Reply 5/5: that's the whole loop - create, join, send, recv. Open a real room with one POST to /room on this server and bring a peer. Ending this practice room now.",
}

const practiceTimeoutLine = "Practice time is up (about 15 minutes) - ending this room now. Open a real one anytime with one POST to /room on this server."

var (
	practiceMu      sync.Mutex
	practiceCreates = map[string]int{}
	practiceTotal   int // creates in the current window, all peers
	practiceWindow  time.Time
	practiceActive  int
)

// trustedClientIP keys the practice limiter by the TCP peer only. Unlike
// clientIP it never reads X-Forwarded-For: that header is client-supplied,
// so keying a rate limit on it would let any caller mint a fresh bucket per
// request. Behind a reverse proxy every visitor shares the proxy's peer
// address; the practice surface accepts that coarsening (the proxy bucket
// plus the process-wide caps below are the real bounds) rather than trust a
// spoofable header. Per-visitor fairness behind a verified trusted proxy is
// a deployment decision, not something this handler may assume.
func trustedClientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func practicePeerEnabled() bool { return os.Getenv("ACQ_PRACTICE_PEER") == "1" }

// practiceAllow reserves a practice slot for ip, or reports refusal. A
// reservation is released by practiceRelease when the create fails or the
// bot goroutine exits.
func practiceAllow(ip string) bool {
	practiceMu.Lock()
	defer practiceMu.Unlock()
	if time.Since(practiceWindow) > time.Hour {
		practiceWindow = time.Now()
		practiceCreates = map[string]int{}
		practiceTotal = 0
	}
	if practiceCreates[ip] >= practicePerIPHour || practiceTotal >= practiceGlobalPerHour || practiceActive >= practiceMaxConcurrent {
		return false
	}
	practiceCreates[ip]++
	practiceTotal++
	practiceActive++
	return true
}

func practiceRelease() {
	practiceMu.Lock()
	practiceActive--
	practiceMu.Unlock()
}

// handlePractice serves POST /practice: mint a room whose seat A is the
// practice bot, and hand the visitor the join path. The seat-A token and
// pair code stay server-side; the response carries only visitor-facing
// fields.
func handlePractice(w http.ResponseWriter, req *http.Request) {
	// POST only, same anti-crawler gate as /room: a GET must never mint.
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonOut(w, http.StatusMethodNotAllowed, map[string]string{"detail": "use POST to open a practice room"})
		return
	}
	ip := trustedClientIP(req)
	if !practiceAllow(ip) {
		jsonOut(w, http.StatusTooManyRequests, map[string]string{"detail": "slow down"})
		return
	}
	code, body := callInternal("POST", "/room", "", map[string]any{
		"topic":           "practice with ACQ Practice (deterministic demo bot)",
		"joinWindowHours": 0.25,
	}, ip, "", req.Host, req.Header.Get("X-Forwarded-Proto"))
	if code != http.StatusOK && code != http.StatusCreated {
		practiceRelease()
		// Never forward the internal body: an ambiguous-commit create (500
		// stored=unknown) carries the fresh room's seat-A token, credential
		// and pair code, which must never reach the visitor. Honest status,
		// static detail, no payload. If the room did commit, no bot runs for
		// it and the visitor never learned its UIN; it expires unclaimed.
		jsonOut(w, code, map[string]string{"detail": "practice room creation failed"})
		return
	}
	var created struct {
		RoomID string  `json:"roomId"`
		Token  string  `json:"token"`
		JoinBy float64 `json:"joinBy"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.RoomID == "" || created.Token == "" {
		practiceRelease()
		jsonOut(w, http.StatusInternalServerError, map[string]string{"detail": "practice room creation failed"})
		return
	}
	go practiceBot(created.RoomID, created.Token, ip)
	base := baseURL(req)
	jsonOut(w, http.StatusOK, map[string]any{
		"practice": true,
		"roomId":   created.RoomID,
		"note":     practiceBotNote,
		"join":     "curl -s -X POST " + base + "/room/" + created.RoomID + "/join  (save the token; then exchange messages per PROTOCOL.md)",
		"joinBy":   created.JoinBy,
	})
}

// practiceBot is the demo seat's loop: read with an explicit since cursor,
// answer each visitor message with the next constant script line, and end
// the room at the script end or the lifetime deadline. Sends are never
// retried, so a failure can drop a line but never duplicate one.
func practiceBot(uin, tokenA, ip string) {
	defer practiceRelease()
	deadline := time.Now().Add(practiceRoomLifetime)
	since := 0
	answered := 0
	bannerSent := false
	for time.Now().Before(deadline) {
		code, body := callInternal("GET", "/room/"+uin+"/msg?since="+strconv.Itoa(since)+"&wait="+strconv.Itoa(practicePollWaitSecs), tokenA, nil, ip, "", "", "")
		if code == http.StatusNotFound || code == http.StatusGone {
			return // room expired or ended
		}
		if code != http.StatusOK {
			time.Sleep(time.Second)
			continue
		}
		var resp struct {
			Messages   []Msg `json:"messages"`
			Cursor     int   `json:"cursor"`
			PeerJoined bool  `json:"peerJoined"`
			Ended      bool  `json:"ended"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			time.Sleep(time.Second)
			continue
		}
		since = resp.Cursor
		if resp.Ended {
			return // visitor (or expiry) ended the room; release the slot now
		}
		if !resp.PeerJoined {
			continue
		}
		if !bannerSent {
			if !practiceSend(uin, tokenA, ip, practiceScript[0], false) {
				practiceSend(uin, tokenA, ip, practiceTimeoutLine, true) // best-effort close
				return
			}
			bannerSent = true
		}
		pending := 0
		for _, m := range resp.Messages {
			if m.From == "B" {
				pending++
			}
		}
		for pending > 0 {
			answered++
			line := practiceScript[answered]
			last := answered >= practiceMaxExchanges
			if !practiceSend(uin, tokenA, ip, line, last) {
				if !last {
					practiceSend(uin, tokenA, ip, practiceTimeoutLine, true) // best-effort close
				}
				return
			}
			if last {
				return
			}
			pending--
		}
	}
	// Deadline reached: best-effort close so the visitor is not left hanging.
	practiceSend(uin, tokenA, ip, practiceTimeoutLine, true)
}

func practiceSend(uin, tokenA, ip, text string, end bool) bool {
	code, _ := callInternal("POST", "/room/"+uin+"/msg", tokenA, map[string]any{"text": text, "end": end}, ip, "", "", "")
	if code != http.StatusOK && code != http.StatusCreated {
		log.Printf("practice bot: send to room %s failed: status %d", uin, code)
		return false
	}
	return true
}
