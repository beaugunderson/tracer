// Package trace runs mtr-style continuous traceroutes over raw ICMP sockets and
// keeps per-hop latency history for rendering.
package trace

import (
	"math"
	"net"
	"sync"
	"time"
)

// SampleState is the lifecycle of a single probe.
type SampleState int

const (
	Pending SampleState = iota // sent, awaiting reply
	OK                         // reply received
	Lost                       // timed out without reply
)

// sample is one probe to one hop.
type sample struct {
	seq    uint16
	round  int // global probe round, shared across hops for sparkline alignment
	sentAt time.Time
	rtt    time.Duration
	state  SampleState
	late   bool // credited after the timeout; excluded from the bar ceiling
}

// hop is one TTL position in the path. Addresses can change between probes
// (load balancing), so the most recently seen one wins for display.
type hop struct {
	ttl      int
	addr     net.IP
	host     string          // reverse-DNS name, falls back to addr when empty
	asn      string          // origin-AS label, empty until resolved
	hostDone bool            // reverse DNS resolved (or permanently unresolvable); retried while false
	asnDone  bool            // ASN resolved (or permanently unresolvable); retried while false
	addrs    map[string]bool // every address ever seen at this TTL
	samples  []*sample       // chronological ring, oldest first

	// Running aggregates over the hop's whole lifetime. The samples ring is
	// capped, so it only backs the sparkline window; the cumulative stat columns
	// come from these, updated as each probe is decided.
	sent     int // decided probes (OK + Lost)
	recv     int // OK probes
	sumRTT   time.Duration
	sumMs    float64 // running Σrtt and Σrtt² in ms, for jitter (stddev)
	sumSqMs  float64
	best     time.Duration
	worst    time.Duration
	last     time.Duration // RTT of the newest-sent OK probe
	lastOKAt time.Time     // send time of the newest-sent OK probe
}

// noteLost counts a probe decided as lost.
func (h *hop) noteLost() { h.sent++ }

// noteOK counts a probe decided as received.
func (h *hop) noteOK(rtt time.Duration, sentAt time.Time) {
	h.sent++
	h.creditOK(rtt, sentAt)
}

// creditOK folds a received probe's RTT into the aggregates without touching the
// sent count — used directly when a probe already counted as lost is answered
// late and flips back to OK.
func (h *hop) creditOK(rtt time.Duration, sentAt time.Time) {
	h.recv++
	h.sumRTT += rtt
	ms := float64(rtt) / float64(time.Millisecond)
	h.sumMs += ms
	h.sumSqMs += ms * ms
	if h.recv == 1 || rtt < h.best {
		h.best = rtt
	}
	if rtt > h.worst {
		h.worst = rtt
	}
	// A late credit can be older than the newest reply; Last and SinceOK track
	// the newest-sent OK probe only.
	if sentAt.After(h.lastOKAt) {
		h.lastOKAt = sentAt
		h.last = rtt
	}
}

const sampleCap = 4096

// DownThreshold is how long without a reply from the destination before it's
// treated as offline — used by the connectivity banner and outage tracking.
const DownThreshold = 1500 * time.Millisecond

const maxOutages = 50

// recentWindow is how many recent replies the displayed latency averages over.
const recentWindow = 5

// Outage is one episode where the destination was unreachable. End is zero while
// the outage is still ongoing.
type Outage struct {
	Start time.Time
	End   time.Time
}

func (o Outage) Ongoing() bool { return o.End.IsZero() }

// Duration is the outage length, using now for an ongoing one.
func (o Outage) Duration(now time.Time) time.Duration {
	if o.Ongoing() {
		return now.Sub(o.Start)
	}
	return o.End.Sub(o.Start)
}

// Session is one address-family traceroute (IPv4 or IPv6) to a single target.
type Session struct {
	Family   string // "IPv4" or "IPv6"
	Target   string // the hostname the user asked for
	TargetIP net.IP

	mu         sync.Mutex
	hops       map[int]*hop
	pending    map[uint16]*pendingProbe
	maxHops    int
	roundNo    int // monotonic probe-round counter, stamped onto each sample
	outages    []Outage
	outageOpen bool
}

type pendingProbe struct {
	ttl    int
	s      *sample
	sentAt time.Time
}

func newSession(family, target string, ip net.IP, maxHops int) *Session {
	return &Session{
		Family:   family,
		Target:   target,
		TargetIP: ip,
		hops:     make(map[int]*hop),
		pending:  make(map[uint16]*pendingProbe),
		maxHops:  maxHops,
	}
}

func (s *Session) getHop(ttl int) *hop {
	h := s.hops[ttl]
	if h == nil {
		h = &hop{ttl: ttl, addrs: make(map[string]bool)}
		s.hops[ttl] = h
	}
	return h
}

// Reset clears all latency history and statistics while keeping the discovered
// path and the outage log, mirroring mtr's "restart statistics".
func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hops {
		h.samples = nil
		h.sent, h.recv = 0, 0
		h.sumRTT, h.sumMs, h.sumSqMs = 0, 0, 0
		h.best, h.worst, h.last = 0, 0, 0
		h.lastOKAt = time.Time{}
	}
	s.pending = make(map[uint16]*pendingProbe)
}

// SampleView is a read-only sample for rendering.
type SampleView struct {
	State SampleState
	RTT   time.Duration
	Round int // global probe round, used to align glyphs across hops
}

// HopView is a read-only snapshot of one hop.
type HopView struct {
	TTL     int
	Host    string
	Addr    string
	ASN     string
	Samples []SampleView // chronological, oldest first, at most maxSamples
	Sent    int
	Recv    int
	LossPct float64
	Last    time.Duration
	Recent  time.Duration // mean RTT over the last few replies (smooths jitter)
	Avg     time.Duration
	Best    time.Duration
	Worst   time.Duration
	Jitter  time.Duration // stddev of RTT over received probes
	SinceOK time.Duration // time since the most recent reply; <0 if never replied
}

// SessionView is a read-only snapshot of a whole traceroute.
type SessionView struct {
	Family    string
	Target    string
	TargetIP  string
	Hops      []HopView
	DestFound bool
	MaxRTT    time.Duration // worst latency across hops, for global scaling
	Outages   []Outage      // recent destination-unreachable episodes, oldest first
}

// Snapshot copies the current state for rendering. maxSamples bounds how many
// recent samples each hop returns (0 means all).
func (s *Session) Snapshot(maxSamples int) SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Derive the path length every snapshot from which hop currently answers as
	// the target (its address equals the target IP). The smallest such TTL is the
	// destination; everything past it is hidden. This tracks route changes live —
	// the path grows, shrinks, and reroutes on its own with no frozen ceiling.
	maxKey, destTTL := 0, 0
	for ttl, h := range s.hops {
		if ttl > maxKey {
			maxKey = ttl
		}
		if h.addr != nil && h.addr.Equal(s.TargetIP) && (destTTL == 0 || ttl < destTTL) {
			destTTL = ttl
		}
	}
	maxTTL := maxKey
	if destTTL > 0 {
		maxTTL = destTTL
	}

	view := SessionView{
		Family:    s.Family,
		Target:    s.Target,
		TargetIP:  s.TargetIP.String(),
		DestFound: destTTL > 0,
	}

	for ttl := 1; ttl <= maxTTL; ttl++ {
		h := s.hops[ttl]
		hv := HopView{TTL: ttl}
		if h == nil {
			view.Hops = append(view.Hops, hv)
			continue
		}
		hv.Addr = ""
		if h.addr != nil {
			hv.Addr = h.addr.String()
		}
		hv.Host = h.host
		if hv.Host == "" {
			hv.Host = hv.Addr
		}
		hv.ASN = h.asn

		samples := h.samples
		if maxSamples > 0 && len(samples) > maxSamples {
			samples = samples[len(samples)-maxSamples:]
		}

		// Cumulative stats come from the hop's running aggregates, which cover the
		// whole run — the samples ring is capped and only backs the sparkline.
		hv.Sent, hv.Recv = h.sent, h.recv
		hv.Last, hv.Best, hv.Worst = h.last, h.best, h.worst
		if hv.Recv > 0 {
			hv.Avg = h.sumRTT / time.Duration(hv.Recv)
			hv.SinceOK = time.Since(h.lastOKAt)
		} else {
			hv.SinceOK = -1
		}
		if hv.Recv > 1 {
			n := float64(hv.Recv)
			if variance := h.sumSqMs/n - (h.sumMs/n)*(h.sumMs/n); variance > 0 {
				hv.Jitter = time.Duration(math.Sqrt(variance) * float64(time.Millisecond))
			}
		}
		// Recent = mean of the last few replies, a steadier "current latency" than
		// the single last ping (which jumps with Starlink's per-ping jitter).
		var rsum time.Duration
		rn := 0
		for i := len(h.samples) - 1; i >= 0 && rn < recentWindow; i-- {
			if h.samples[i].state == OK {
				rsum += h.samples[i].rtt
				rn++
			}
		}
		if rn > 0 {
			hv.Recent = rsum / time.Duration(rn)
		}
		if hv.Sent > 0 {
			hv.LossPct = 100 * float64(hv.Sent-hv.Recv) / float64(hv.Sent)
		}

		hv.Samples = make([]SampleView, len(samples))
		for i, sm := range samples {
			hv.Samples[i] = SampleView{State: sm.state, RTT: sm.rtt, Round: sm.round}
			// The height scale tracks only the visible window, so it recovers
			// once an old spike scrolls off (stats columns keep full history).
			// Late-credited replies are excluded: a hop whose control plane
			// answers tens of seconds late (Starlink's PoP gateway does this
			// continuously) would otherwise pin the ceiling and flatten every
			// other row's bars. Its own row still shows them — full-height bars,
			// and honest loss/avg/worst columns.
			if sm.state == OK && !sm.late && sm.rtt > view.MaxRTT {
				view.MaxRTT = sm.rtt
			}
		}
		view.Hops = append(view.Hops, hv)
	}

	if len(s.outages) > 0 {
		view.Outages = make([]Outage, len(s.outages))
		copy(view.Outages, s.outages)
	}
	return view
}

// updateOutagesLocked records destination-unreachable episodes; callers hold mu.
func (s *Session) updateOutagesLocked(now time.Time) {
	down, lastOK := s.destDownLocked(now)
	switch {
	case down && !s.outageOpen:
		start := now
		if !lastOK.IsZero() {
			start = lastOK // the moment the destination went silent
		}
		s.outages = append(s.outages, Outage{Start: start})
		if len(s.outages) > maxOutages {
			s.outages = s.outages[len(s.outages)-maxOutages:]
		}
		s.outageOpen = true
	case !down && s.outageOpen:
		s.outages[len(s.outages)-1].End = now
		s.outageOpen = false
	}
}

// destDownLocked reports whether the current destination hop (smallest TTL whose
// address is the target) has gone longer than DownThreshold without a reply, plus
// the time of its last reply. Callers hold mu.
func (s *Session) destDownLocked(now time.Time) (bool, time.Time) {
	destTTL := 0
	for ttl, h := range s.hops {
		if h.addr != nil && h.addr.Equal(s.TargetIP) && (destTTL == 0 || ttl < destTTL) {
			destTTL = ttl
		}
	}
	if destTTL == 0 {
		return false, time.Time{} // no destination known yet — not an outage
	}
	h := s.hops[destTTL]
	if h.recv > 0 {
		return now.Sub(h.lastOKAt) >= DownThreshold, h.lastOKAt
	}
	// No reply on record. Probes decided as lost are evidence of an outage; an
	// empty record (e.g. right after a stats reset) is unknown, not an outage.
	return h.sent > 0, time.Time{}
}
