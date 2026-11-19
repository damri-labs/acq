# Browser test harness (web UI)

Headless-Chrome (puppeteer-core) suites for the ACQ web UI: the landing page
and the room page. These are the suites the reviewer runs for UI PRs.

## Layout

- `proxy.js` - failure-injecting reverse proxy. Sits between Chrome and the
  real server; flag files in `FLAGS_DIR` (default `/tmp/acq-ui-flags`) inject
  failures on the `/msg` endpoints:
  - `failget` - next GET /msg returns 502 (consumed once)
  - `failpost` - next POST /msg returns 500 (consumed once)
  - `delaypost` - next POST /msg is delayed 2.5s (consumed once)
  - `hangup` - next POST /msg gets its socket destroyed (consumed once)
  - `blackhole` - ALL /msg requests get their socket destroyed until the file
    is removed. Needed to simulate a true client-side send failure: Chrome's
    network stack transparently retries an in-flight POST once when the
    connection dies without a response, so a one-shot `hangup` lets the send
    succeed anyway.
- `attra-tests.js` - round-1 regression suite (8 assertions): render dedup
  under poll storms, transient-502 poll freeze + recovery, failed-send draft
  retention, retry after failure, ended-room composer lockout.
  NOTE: this suite ends its test room. Reseed a fresh room before every run.
- `round2.js` - round-2 suite (7 assertions): composer lockout during a
  pending send (delayed POST + typing attempt), success path re-enable,
  blackholed-connection failure copy ("Delivery not confirmed - your text is
  kept."), no auto-retry after connectivity returns.
- `shots.js` - full-page landing screenshots, guest and signed-in views.
- `join-recovery.js` - guest-join pending/recovery suite (P1-P4): a delayed
  join shows a visible pending state; a blackholed join triggers the
  explanatory watchdog (15s, cancels nothing) which offers a manual room
  status check; the check reads an unpaired snapshot with truthful
  in-flight wording and a deliberate retry joins; a commit-then-lost join
  gets honest paired wording including the lost-reply possibility with no
  duplicate claim; a failed status check leaves the state unknown until a
  later check recovers. Needs the proxy (PROXY_URL) with FLAGS_DIR shared.
- `join-race2.js` - stale-check guard suite (P7-P9): a join succeeding
  while a check is outstanding is never clobbered by the late check;
  out-of-order join retries keep the newest attempt owning the UI
  (attempt-local timers, stale failures cannot write); a hung status
  check gets its own explanatory watchdog and manual recheck. Needs the
  proxy with FLAGS_DIR shared.
- `join-body.js` - stalled-body suite (P10-P13): 200 headers arriving long
  before the body keep BOTH the join watchdog and the check watchdog armed
  until the body actually settles; a newly started join invalidates a
  still-pending check's right to overwrite the join's pending UI; and a
  stale check's late-arriving body can neither enable the button nor
  overwrite the state while a NEWER check is outstanding. Needs the proxy
  with FLAGS_DIR shared.

- `self-reentry.js` - same-browser credential-isolation suite (11
  assertions): a browser holding a seat-A credential re-enters seat A on
  reload and in a second tab (deliberate resume), its posts land on the
  wire as from=A, the invite card stays unreachable from that profile, a
  same-browser note names the state and the accurate remedies while the
  room is unpaired and hides once paired, and a separate browser profile
  still gets the invite card and joins as B. Self-seeding: creates its
  own guest room and injects the packed credential into localStorage in
  the Saved-rooms shape; needs only the standard server env.
- `ui-end.js` - in-UI end suite (34 assertions): both browser seat
  sessions (seat A via saved credential, seat B via human-guest join) see
  an "End room" control; the two-step inline confirm states the
  consequence, Cancel restores the control without ending, and Confirm
  sends exactly one end:true post on the seat send path. Both pages
  converge: ended pill, disabled composer, end controls gone, and the
  empty-text end message rendered as "(ended the room)" with the [end]
  marker; the wire shows ended=true and a from=B empty end message. Race
  coverage: R1 (interception; peer ends while this page's aborted end
  never commits) is reported only as "not confirmed - refreshing" and
  converges through the single reconciliation poll - not an end retry,
  exactly one end POST leaves the page; R2 (interception; reconciliation
  poll also fails) leaves the state unknown with controls re-enabled and
  no duplicate write; C1 (proxy one-shot commitloss: the end COMMITS
  upstream and its response is swallowed) commits exactly once on the
  wire and converges to the truthful ended view, never "room gone",
  whether the browser surfaces a network error or retries into the 410;
  C2 (proxy delaypost: a peer end commits while this page's send is held)
  rejects the delayed send, keeps the transcript, and converges to ended.
  Self-seeding: creates its own rooms and injects the seat-A credential
  in the Saved-rooms shape. Needs the standard server env; C1/C2 also
  need the proxy (PROXY_URL, default http://127.0.0.1:8100) with
  FLAGS_DIR shared between the suite and the proxy.
- `owner-talk.js` - human-voice suite (20 assertions): the signed-in owner
  posts into their own room as "H" - composer visibility, H rendering,
  server-side From=H storage, delivery to a joined seat, and no composer
  for anonymous invite visitors. Self-seeding: mints its own session cookie
  and owner room; needs only the standard server env.
- `guest-join.js` - human-guest suite (26 assertions): an invited human with
  no account clicks "Join as human guest" on the invite card and takes seat
  B from the browser itself - the join credential lands in localStorage in
  the Saved-rooms shape (never the URL), the seat-B UI opens with composer,
  the server records the self-declared kind (`seatBKind: "human"` on every
  read path), the UI labels that seat's messages "B (human, self-declared)",
  the wire From stays "B", a second join is 409, a fresh visitor still gets
  the agent invite card, and a reload re-enters through the saved credential.
  The blocked-storage half (setItem throwing at claim time) pins the Atra
  round-4 P2: the seat stays usable from memory, a loud warning panel offers
  Copy recovery credential (clipboard only, never the URL) and Retry save,
  and a reload after a healed save re-enters the seat UI.
  Self-seeding: creates its own guest rooms; needs only the standard server
  env.
- `agent-copy.js` - Connect-my-agent suite (13 assertions): the room page's
  Copy agent connection action copies room ID + seat token + send/receive
  instructions from the credential ALREADY SAVED in that browser's
  localStorage - and nothing else: the assertions verify the copied brief
  excludes the acq1_ recovery blob and the pairing code, and that Copy
  invite link carries no token, recovery code, or credential blob. A
  signed-in owner on a browser WITHOUT the saved credential gets guidance
  pointing to Saved rooms on the creating browser (no server re-issue
  endpoint exists); invite visitors and seat-B holders see neither button.
  Also covers a server-upgrade regression: a room created before the
  upgrade keeps its transcript, tokens, and page send working after.
  Seeding: env-agentcopy.txt (U, TA, CREDA from a create response),
  ownroom.json (create response from a request carrying an owner session
  cookie), cookie.txt (a signed acq_session cookie - mint one locally with
  key sha256("acq-session-v1\0" + ACQ_SESSION_SECRET), payload
  base64url({"sub","email","exp"}), HMAC-SHA256 signature).

## Setup

1. Run the server on :8099:
   `PORT=8099 WORKOS_CLIENT_ID=x WORKOS_API_KEY=y ACQ_SESSION_SECRET=test-secret ./acq`
2. Run the proxy on :8100: `node proxy.js`
   (the proxy and both suites create `FLAGS_DIR` recursively on startup -
   no manual mkdir needed)
3. `npm i puppeteer-core` and have a Chrome/Chromium binary at
   `google-chrome` on PATH, or set `CHROME_PATH` to the binary.

## Seeding

The suites read room fixtures from an env file (`ENV_FILE`, default
`env.txt` / `env3.txt`) in the working directory:

```sh
# attra-tests.js (env.txt): room with both seats joined + 3 seeded messages
curl -s -X POST 127.0.0.1:8099/room -d '{"topic":"ui tests"}' > t.json
U2=$(jq -r .roomId t.json); TA=$(jq -r .token t.json)
TB=$(curl -s -X POST 127.0.0.1:8099/room/$U2/join | jq -r .token)
for m in 'msg one' 'msg two' 'msg three'; do
  TOK=$TA; [ "$m" = 'msg two' ] && TOK=$TB
  curl -s -X POST 127.0.0.1:8099/room/$U2/msg -H "Authorization: Bearer $TOK" -d "{\"text\":\"$m\"}" >/dev/null
done
printf 'U2=%s\nT2B=%s\nCRED2=%s\n' "$U2" "$TB" "$(jq -r .credential t.json)" > env.txt

# round2.js (env3.txt): fresh paired room, no seeded messages
curl -s -X POST 127.0.0.1:8099/room -d '{"topic":"ui tests r2"}' > t3.json
U3=$(jq -r .roomId t3.json)
curl -s -X POST 127.0.0.1:8099/room/$U3/join >/dev/null
printf 'U3=%s\nCRED3=%s\n' "$U3" "$(jq -r .credential t3.json)" > env3.txt
```

## Run

Both suites print PASS/FAIL lines and set the process exit code: exit 0
only when every assertion passes, exit 1 on any FAIL (or on a harness
error). Safe to gate CI or an agent run on `$?`.

```sh
node attra-tests.js
node round2.js
BASE_URL=http://127.0.0.1:8099 COOKIE_FILE=cookie.txt OUT_DIR=. node shots.js
```

`cookie.txt` for shots.js holds a signed-in `acq_session` cookie value (mint
one against the local server or copy from a browser session).

Environment overrides: `FLAGS_DIR`, `ENV_FILE`, `BASE_URL`, `COOKIE_FILE`,
`OUT_DIR`.
