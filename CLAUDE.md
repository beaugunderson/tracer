# tracer — context for Claude

mtr-style continuous traceroute in Go. Braille-sparkline per-hop latency history,
simultaneous IPv4 + IPv6 paths. Single binary, bubbletea TUI.

## Layout

- `main.go` — flags, target, root check (only for `-r`), launches TUI or `-report`.
- `internal/trace/` — the traceroute engine and shared state.
  - `session.go` — `Session` (per address family), per-hop sample ring buffers
    plus per-hop **running aggregates** (sent/recv/sum/sumSq/best/worst/last/
    lastOKAt, updated via `noteOK`/`noteLost`/`creditOK` as each probe is
    decided). The ring (`sampleCap` 4096) only backs the sparkline window; every
    cumulative stat column comes from the aggregates, so nothing degrades into a
    rolling window on long runs and `Snapshot` never scans full history.
    `Snapshot(maxSamples)` returns the exported read-only `SessionView`/`HopView`
    used by the UI; `Reset()` clears samples + aggregates but keeps the
    discovered path AND the outage log.
  - `engine.go` — sockets, probe send loop, receive loop, loss sweeper, reverse
    DNS + ASN resolution (with a 30s retry loop for hops still unresolved).
- `internal/sparkline/` — braille rendering. `Cells([]Point) []Cell`; one glyph = 2
  samples, bars 0–4 dots filled from the bottom. Loss = full bar, `Cell.Loss` set.
- `internal/ui/` — bubbletea `Model` + pure `renderFrame(...)` (testable without a
  TTY) and `Report(...)` for `-report` plain-text mode.

## Two-channel encoding

- Bar **height** is a **per-family** latency scale (that family's worst hop sets
  its ceiling), shown in each session header (`bars 0–Xms`) — read *where* latency
  enters that path without a slow family flattening a fast one. The ceiling
  (`SessionView.MaxRTT`) is computed over only the **visible window**, not full
  history, so it recovers once an old spike scrolls off; the avg/best/worst stat
  columns stay cumulative (they read the hop's running aggregates, not the
  capped ring).
- Glyph **color** is an independent per-row gradient over that hop's own min→max
  (`colorIndexFor`, `gradientPalette`), so a globally-flat hop still shows its
  jitter. The ramp is cool→warm and deliberately avoids red; **red is reserved for
  loss**. A glyph holds two pings but a cell has one color, so it's colored by the
  worse of the two. `colorFloor` keeps sub-ms jitter from filling the whole ramp.
  Toggle with `-mono` (`ui.New(sessions, color, asn)`). `renderRuns` applies one
  style per run of equal-colored glyphs to keep ANSI escapes down.
- When color is off (`-mono`, `-report`), a loss cell renders as `×` (`lossRune`)
  instead of the braille full bar — without red, `⣿` would be indistinguishable
  from ceiling-height latency. Color mode keeps the red `⣿`.

## Engine specifics

- Default sockets are **datagram ICMP** (`udp4`/`udp6`) — no root needed. `-r` uses
  raw (`ip4:icmp`/`ip6:ipv6-icmp`), which needs root.
- TTL is set with the **`SetTTL`/`SetHopLimit` socket option**, not a per-write
  control message: macOS datagram sockets silently ignore the control-message TTL.
  Safe because every probe is sent from the single `sendLoop` goroutine.
- Datagram mode: the kernel rewrites the echo ID to the socket port (in the reply
  and in the packet quoted inside Time Exceeded), so the id used for matching is the
  local UDP port; raw mode uses the pid.
- Probes match replies by sequence number via the `pending` map. `hop.samples` is
  `[]*sample` (pointers) so trimming the ring never invalidates pending references.
- The sweeper (`sweep`, called by `sweepLoop` every 200ms) decides probes in two
  phases: past the adaptive **RTO** a probe is marked `Lost` (so loss shows
  promptly) but **stays in `pending`**; past `lateGrace` (60s, or 2×Timeout if
  larger) it's pruned. A reply inside the grace flips the sample back to `OK` via
  `hop.creditOK` — recv/avg/best/worst/loss% self-repair; `Last`/`SinceOK` only
  move if the credited probe is the newest-sent OK. The grace must stay well
  under the ~18-minute uint16 seq-wrap horizon. Replies at or past
  `queuedThreshold` (3s) are excluded from the family bar ceiling (`MaxRTT`): a
  hop whose control plane answers ~30s late *continuously* (Starlink's PoP
  gateway) would otherwise pin the ceiling and flatten every other row. Its own
  row still shows them (full-height bars, honest loss/avg/worst).
- The lost-timeout is **adaptive** (`Session.rtoLocked`): a per-hop Jacobson/Karels
  estimator (`hop.updateRTO`, srtt + 4·rttvar) trained on that hop's prompt replies
  (those under `queuedThreshold`, so a chronically-queued hop can't inflate it),
  and the path RTO is the **max across hops** so the slowest legit hop sets the
  bar and no honest hop is timed out early. Clamped to [`rtoFloor` 250ms, the
  user's `-t`]: `-t` is the ceiling the timeout only tightens *below*, never
  loosens past; falls back to the ceiling until a hop has a prompt reply. This is
  why loss shows in a fraction of a second on a fast path instead of after 2s.
- A hop is flagged `HopView.Deprioritized` when queued replies (past
  `queuedThreshold`) are the majority of its replies (`h.queuedRecv/h.recv ≥
  deprioRatio` 0.5, with `recv ≥ deprioMinRecv` 10). The threshold is a fixed
  absolute bound, not relative to the (adaptive) RTO or to other hops, so a
  genuinely slow-but-prompt link never trips it and the detection reference stays
  stable while the timeout adapts around it. The UI (`renderHop`) greys the whole
  row — sparkline included (rendered mono via `sparkRunes`, loss as `×`, then
  dimmed) — and replaces the misleading latency+jitter columns with the
  `deprioTag` (`deprioritized`). `-report` keeps the raw numbers.
- `extractEcho` also checks the quoted inner packet's ICMP type byte (echo
  request: 8 v4 / 128 v6), so quoted traffic from another program colliding on
  id can't match.
- `-4`/`-6` apply to literal IPs too; a family flag contradicting a literal is a
  `permanentError`, which `resolveRetry` fails fast on instead of retrying.
- `sendLoop` probes the whole TTL range (`1..maxHops`) **every** cycle, paced
  `interval/maxHops` apart — never clamped. So the whole path appears within ~one
  round-trip AND the path tracks route changes live (grows/shrinks/reroutes). The
  cost is a few echoes to TTLs past the destination each cycle; cheap.
- The displayed path length is derived **per snapshot** via `destTTLLocked`: the
  destination is the smallest TTL whose hop address equals `TargetIP` **with
  fresh evidence** (last reply within `destChooseWindow` of the freshest
  target-labeled hop's). Freshness matters: the target occasionally answers a
  low-TTL probe for real (Starlink CGNAT re-originating packets with a fresh
  TTL during congestion), and if that TTL's real router is silent the label
  never gets overwritten — a plain smallest-TTL rule then collapses the path
  there permanently and fakes an outage (false OFFLINE). During a real outage
  all candidates are equally stale, so all pass and the choice is stable. A
  target-labeled hop below the chosen TTL whose evidence lags by
  `destClearAfter` (60s) is provably mislabeled and has its address cleared.
  `destDownLocked` (outage tracking) uses the same selection. Hops past the
  destination are hidden; `DestFound` = a destination was chosen.
- Every probe is stamped with a global round number (`Session.roundNo`). The UI
  renders the sparkline by bucketing each ping into glyph `round/2` (even round →
  left dot, odd → right dot) — see `renderSpark`. This keeps a ping in the same
  column across frames (no jitter), aligns every hop to the same columns regardless
  of when it was discovered, shows the very first reply immediately, and leaves
  rounds with no sample blank (a hop missing the newest round shows a blank right
  edge until its reply lands — live + aligned). Do not re-introduce a Snapshot-side
  even-round drop; it hid the first reply.
- The right-edge round is computed **per family** in `renderSession`, NOT globally:
  each `Session` has its own `roundNo` and they drift, so a shared `maxRound` leaves
  a gap on whichever family is a round behind. The latency ceiling stays global.
- `View` pads the frame to the viewport height and `Update` returns `tea.ClearScreen`
  on `WindowSizeMsg` — bubbletea's line-diff renderer otherwise leaves stale rows
  after a resize (notably when the tab was backgrounded and the resize was missed).
- A reply only sets a hop's address from a source that legitimately responded:
  Echo Reply and Time Exceeded always, but a Destination Unreachable only when its
  source **is the target**. A Dest Unreachable from an *intermediate* router
  (common on IPv6 during a Starlink dropout) is ignored, so it can't mislabel a
  hop or make a low TTL look like the destination.
- Probes are **paced** across the interval (one hop at a time, `interval/ceiling`
  apart), not bursted, so each row's reply lands at a different moment and the rows
  update live — matching mtr.
- The sparkline is **right-aligned** with the newest ping at the right edge and
  blank space on the left until history fills, mirroring mtr's `saved[]` buffer
  (`ui/curses.c`: `max_cols = min(maxx - padding, SAVED_PINGS)`, loop
  `for (i = SAVED_PINGS - cols; i < SAVED_PINGS; i++)`). Do not left-align.
- Consecutive non-responding hops (no address, zero replies) collapse into one dim
  `(no reply)` summary line (`isSilent`/`renderSilent`). mtr instead shows red
  `???`; the collapse is a deliberate deviation.
- Bogus reverse-DNS names (`undefined.hostname.localhost`, `*.in-addr.arpa`, no dot,
  etc.) are rejected by `isBogusPTR` so the row falls back to the raw IP.
- ASN per hop (`-z` → `Options.LookupASN`): `asn.go` resolves the origin AS via
  Team Cymru's DNS service (two TXT lookups: `*.origin[6].asn.cymru.com` for the
  number, `AS<n>.asn.cymru.com` for the name), on the same async path as reverse
  DNS, with the AS-name cached (`asnNameCache`) and non-routable IPs skipped
  (`routableForASN`). The label is the name only (number is dropped; falls back to
  `AS<n>` only when unnamed); `asnRename` shortens known handles (SPACEX-STARLINK →
  STARLINK). The UI shows a dim 8-wide `colASN` column (ellipsized) when `-z` is set.
- Reverse-DNS and ASN lookups distinguish **settled** from transient outcomes:
  a hop keeps `hostDone`/`asnDone` false after a timeout/servfail (DNS down
  mid-dropout) and `resolveLoop` re-kicks those every 30s until they settle; a
  bogus PTR, NXDOMAIN, or non-routable address settles immediately. `asnName`
  never caches a failed lookup, so a transient miss can't pin an empty name for
  the whole run. An address change resets both flags.

## Resilience (built for leaving running through Starlink dropouts)

- `recvLoop` must never `return` on a non-timeout socket error — a network-down
  error mid-outage would permanently kill the receiver for that family. It backs
  off ~200ms and keeps listening; recovery is automatic when the link returns.
- Startup DNS resolves via `resolveRetry`, which retries until it succeeds or the
  context is cancelled (each attempt capped at 5s, paced 1s apart) so launching
  during a dropout doesn't fail hard — a Starlink cold boot can leave DNS down for
  minutes. It calls the `onResolve` callback before each attempt; `main.go` uses it
  to print an in-place `resolving <host>… (attempt N: <reason>)` line to stderr so
  the wait isn't a blank screen. A real error (typo'd host) shows as a repeating
  message the user can Ctrl-C out of.
- The connectivity banner (`renderStatus`/`familyStatus`) keys off `HopView.SinceOK`
  (time since the destination's last reply), not the loss sweeper, so OFFLINE shows
  ~`trace.DownThreshold` (1.5s) after the last good packet rather than after the 2s
  per-probe timeout. ONLINE if any family is reachable (Zoom works on either).
- Outages are tracked engine-side in `sweepLoop` via `updateOutagesLocked` (keyed on
  the same `DownThreshold` as the banner), stored per `Session` and exposed as
  `SessionView.Outages`. `renderOutages` lists recent episodes below the chart
  (and `renderReport` in `-report` output — both share `outageTable`), annotating
  a family with `≈ every Ns` only when `periodicity()` finds the onset intervals
  regular (≥4 outages, coefficient of variation < 0.4) — never guesses.
  `shortDur` has an hours tier (`3h05m`) for overnight runs.
- `destDownLocked` reads the dest hop's aggregates: replies on record → compare
  `lastOKAt` against `DownThreshold`; only lost probes on record → down; nothing
  decided at all → **unknown, not down**. That last case is what keeps `Reset()`
  (which empties the aggregates but keeps hop addresses and the outage log) from
  fabricating a phantom outage in the ~interval before the next reply lands.
- `renderFrame` takes the viewport height and sheds when the frame is too tall:
  outage list first, then the footer legend, then hop rows collapse into a dim
  `…N more` line — the title and connectivity banner always survive. height ≤ 0
  (tests) disables the clamp; `View` still pads short frames.
- Jitter is RTT stddev over a hop's received probes (`HopView.Jitter`, computed in
  `Snapshot`); availability in each family header is `100 - destHop.LossPct`.
- The TUI latency column and the banner show `HopView.Recent` — the mean of the
  last `recentWindow` (5) replies — not the single last ping, so the number doesn't
  jump with Starlink's per-ping jitter. The raw single `Last` is kept for `-report`.

## Gotchas learned

- The engine goroutines must run on the long-lived context, not a resolve-timeout
  context — cancelling the latter kills all probing after the first send.
- Little Snitch (or any per-app macOS firewall) prompts separately for each new
  binary; an un-answered prompt silently drops all ICMP and looks like a code bug.

## Verify

- `go test ./...` — sparkline mapping + TUI frame rendering.
- `./tracer -report 12s -i 300ms google.com` — headless end-to-end, no TTY needed.
