# tracer

An mtr-style continuous traceroute with braille-sparkline latency history and
simultaneous IPv4 + IPv6 paths.

```
tracer  kaiton.local  ·  last 280 pings                    2026-06-25T22:47:30-0700

IPv4 → google.com (142.250.72.46)
 1. gateway                          0%   3.2ms  ⣀⣀⣀⣀⣿⣇⣿⣸⣇⣿⣸⣿⣸⣇⣿⣸⣿⡀⢀
 2. 100.64.0.1                       0%  42.8ms  ⣤⣤⣠⣤⣠⣄⣠⣄⣤⣤⣄⣠⣄⣠⣠⣤⣠⣤⣀
 3. (waiting for reply)            100%     --   ⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿
 ...

IPv6 → google.com (2607:f8b0:400a:809::200e)
 1. customer.sttlwax1.isp.starlin…   0%   7.4ms  ⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀
 ...

scale 0–102ms   ⣀ low → ⣿ high   ⣿ = packet loss   q quit · r reset stats
```

## Why it's different from mtr

- **Braille sparklines instead of a color key.** Each row is a dense sparkline:
  one braille glyph encodes two consecutive pings as vertical bars 0–4 dots tall,
  so a row shows twice as many samples as columns. Bar height is the latency on a
  **single global scale** (the worst hop sets the ceiling), so you can read *where*
  latency enters the path at a glance — no legend to memorize. Lost probes render
  as full-height bars in red. Like mtr, history is right-aligned with the newest
  ping pinned to the right edge; probes are paced across the interval so each row's
  newest column fills in live as its reply lands.
- **Two scales at once.** Bar *height* uses a per-family scale (that family's worst
  hop sets the ceiling, shown in the header) so you see where latency enters the
  path without a slow family flattening a fast one. *Color* is an independent
  per-row gradient (cool → warm over each hop's own min→max), so you can also spot
  a hop's jitter even when it's globally flat. Red is reserved for packet loss.
  Disable the gradient with `-mono`.
- **Non-responding hops collapse.** Consecutive routers that never answer (ICMP
  filtered) collapse into one dim `(no reply)` line instead of a wall of red.
- **Origin AS per hop (`-z`).** Annotates each hop with the network that owns it
  (via Team Cymru), so the handoff between providers — e.g. `STARLINK` → `GOOGLE` —
  is obvious, and you can see whether IPv4 and IPv6 transit different networks.
- **Glanceable connectivity banner.** A bold `● ONLINE` / `◐ UNSTABLE` /
  `● OFFLINE 4s` line at the top, driven by how long since the destination last
  replied — so you get an at-a-glance "is it down right now, and for how long" cue
  without reading the sparklines. ONLINE if *either* address family is reachable.
- **Jitter column + availability.** Each hop shows RTT stddev (`±8ms`) — often the
  first sign a call's about to get choppy, before any loss — and each family header
  shows its destination availability (`98.7% up`).
- **Outage log.** Recent destination-unreachable episodes are listed below the
  chart (family, duration, time-ago). When a family's outages recur regularly
  (Starlink's satellite cadence often does), it notes the period: `v6 ≈ every 15s`.
- **Survives outages.** Built to be left running through dropouts: a transient
  network error never kills the receiver, and startup DNS retries through a gap, so
  it keeps monitoring and recovers on its own when the link returns.
- **Dual-stack at once.** When a host resolves to both A and AAAA records, tracer
  runs two independent traceroutes — IPv4 on top, IPv6 below — so path-dependent
  problems (one family slow or broken) are immediately visible.
- **No sudo required.** By default tracer uses unprivileged datagram ICMP sockets
  (the same mechanism `ping` uses). Pass `-r` to use raw sockets where datagram
  ICMP isn't permitted (some Linux configs); that mode needs root.

## Usage

```
tracer [flags] <host>

  -i  interval between full probe cycles (default 0.5s)
  -t  timeout before a probe counts as lost (default 2s)
  -m  maximum number of hops (default 30)
  -4  IPv4 only
  -6  IPv6 only
  -r  use raw ICMP sockets (needs root)
  -mono  disable the per-row latency color gradient
  -z  show each hop's origin AS number + name (Team Cymru)
  -report <dur>  run headless for the duration, print a plain-text report, exit
```

Keys while running: `q` quit, `r` reset statistics.

## Build

```
go build -o tracer .
```

Requires Go 1.26+.
