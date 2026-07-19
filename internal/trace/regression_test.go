package trace

import (
	"net"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// TestResetDoesNotFabricateOutage guards the phantom-outage bug: pressing reset
// while online empties every hop's sample ring, and the sweeper must read that
// empty record as "unknown", not as "the destination stopped answering".
func TestResetDoesNotFabricateOutage(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	h := s.getHop(1)
	h.addr = target

	now := time.Now()
	h.samples = append(h.samples, &sample{state: OK, rtt: time.Millisecond, sentAt: now})
	h.noteOK(time.Millisecond, now)
	s.updateOutagesLocked(now)
	if len(s.outages) != 0 {
		t.Fatalf("healthy session should have no outages")
	}

	s.Reset()

	// The sweeper fires before any fresh reply has landed.
	s.mu.Lock()
	s.updateOutagesLocked(now.Add(200 * time.Millisecond))
	got := len(s.outages)
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("Reset fabricated %d phantom outage(s)", got)
	}
}

// TestResetKeepsOutageHistory: reset restarts statistics, not the outage log.
func TestResetKeepsOutageHistory(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(8, 8, 8, 8), 30)
	now := time.Now()
	s.outages = append(s.outages, Outage{Start: now.Add(-time.Minute), End: now.Add(-50 * time.Second)})

	s.Reset()

	if len(s.Snapshot(10).Outages) != 1 {
		t.Fatalf("Reset dropped the outage history")
	}
}

// TestExtractEchoRequiresEchoType: an ICMP error quoting something other than an
// echo request (e.g. another process's traffic colliding on id) must not match.
func TestExtractEchoRequiresEchoType(t *testing.T) {
	quoted := quotedV4Echo(1234, 7)
	quoted[20] = 3 // inner type: destination unreachable, not echo request
	if _, _, ok := extractEcho(quoted, protoICMP4); ok {
		t.Fatalf("extractEcho accepted a quoted packet that is not an echo request")
	}
	if _, _, ok := extractEcho(quotedV4Echo(1234, 7), protoICMP4); !ok {
		t.Fatalf("extractEcho rejected a genuine quoted echo request")
	}
}

// TestLateReplyCredited: a reply arriving after the timeout marked the probe lost
// must flip it back to OK and repair the loss statistics.
func TestLateReplyCredited(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4, opts: Options{Timeout: 2 * time.Second}}

	sm := seedPending(s, 3, 9)
	sm.sentAt = time.Now().Add(-3 * time.Second) // older than the timeout
	s.pending[9].sentAt = sm.sentAt

	e.sweep(time.Now())
	if sm.state != Lost {
		t.Fatalf("sweeper should have marked the probe lost, state=%v", sm.state)
	}
	if hv := s.Snapshot(10).Hops[2]; hv.LossPct != 100 {
		t.Fatalf("expected 100%% loss after sweep, got %.0f%%", hv.LossPct)
	}

	// The reply straggles in afterwards.
	msg := &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quotedV4Echo(1234, 9)}}
	e.handle(msg, &net.IPAddr{IP: net.IPv4(10, 0, 0, 1)})

	hv := s.Snapshot(10).Hops[2]
	if sm.state != OK {
		t.Fatalf("late reply not credited, state=%v", sm.state)
	}
	if hv.LossPct != 0 || hv.Recv != 1 {
		t.Fatalf("late credit did not repair stats: loss=%.0f%% recv=%d", hv.LossPct, hv.Recv)
	}
}

// TestLateReplyExpiresAfterGrace: a probe older than the grace window is pruned
// and can no longer be credited.
func TestLateReplyExpiresAfterGrace(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4, opts: Options{Timeout: 2 * time.Second}}

	sm := seedPending(s, 3, 9)
	sm.state = Lost
	s.getHop(3).noteLost()
	s.pending[9].sentAt = time.Now().Add(-2 * time.Minute) // far past the grace window

	e.sweep(time.Now())
	if s.pending[9] != nil {
		t.Fatalf("expired probe still creditable after the grace window")
	}

	msg := &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quotedV4Echo(1234, 9)}}
	e.handle(msg, &net.IPAddr{IP: net.IPv4(10, 0, 0, 1)})
	if sm.state != Lost {
		t.Fatalf("reply past the grace window should be ignored, state=%v", sm.state)
	}
}

// TestCumulativeStatsSurviveRingTrim: the samples ring caps the sparkline window,
// but sent/recv/avg/best/worst cover the whole run via running aggregates.
func TestCumulativeStatsSurviveRingTrim(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(1, 2, 3, 4), 30)
	h := s.getHop(1)
	const n = sampleCap + 500
	addSamples(h, 1, n)

	hv := s.Snapshot(10).Hops[0]
	if hv.Sent != n || hv.Recv != n {
		t.Fatalf("stats should be cumulative past the ring cap: sent=%d recv=%d, want %d", hv.Sent, hv.Recv, n)
	}
	if len(h.samples) != sampleCap {
		t.Fatalf("ring should stay capped at %d, got %d", sampleCap, len(h.samples))
	}
}

// TestResolveLiteralRespectsFamilyFlags: -4/-6 apply to literal IPs too, and the
// mismatch fails fast instead of retrying forever.
func TestResolveLiteralRespectsFamilyFlags(t *testing.T) {
	ctx := t.Context()
	if _, _, err := resolveRetry(ctx, "2001:db8::1", Options{Force4: true}, nil); err == nil {
		t.Fatalf("-4 with an IPv6 literal should error")
	}
	if _, _, err := resolveRetry(ctx, "192.0.2.1", Options{Force6: true}, nil); err == nil {
		t.Fatalf("-6 with an IPv4 literal should error")
	}
	if ip4, _, err := resolveRetry(ctx, "192.0.2.1", Options{Force4: true}, nil); err != nil || ip4 == nil {
		t.Fatalf("matching family literal should resolve: ip4=%v err=%v", ip4, err)
	}
}

// TestLateCreditExcludedFromCeiling: a late-credited reply repairs loss/avg/worst
// but must not raise the sparkline height ceiling (MaxRTT) — a hop whose control
// plane answers 30s late would otherwise flatten every other row's bars.
func TestLateCreditExcludedFromCeiling(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4, opts: Options{Timeout: 2 * time.Second}}

	addOK(s.getHop(1), 1, 40*time.Millisecond, time.Now())

	sm := seedPending(s, 1, 5)
	sm.round = 2
	sm.sentAt = time.Now().Add(-28 * time.Second)
	s.pending[5].sentAt = sm.sentAt
	e.sweep(time.Now())
	msg := &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quotedV4Echo(1234, 5)}}
	e.handle(msg, &net.IPAddr{IP: net.IPv4(10, 0, 0, 1)})
	if sm.state != OK {
		t.Fatalf("late reply not credited, state=%v", sm.state)
	}

	v := s.Snapshot(10)
	if v.MaxRTT > 100*time.Millisecond {
		t.Errorf("late-credited reply inflated the bar ceiling: MaxRTT=%v, want ~40ms", v.MaxRTT)
	}
	if w := v.Hops[0].Worst; w < 20*time.Second {
		t.Errorf("Worst should still record the late reply's RTT, got %v", w)
	}
	if lp := v.Hops[0].LossPct; lp != 0 {
		t.Errorf("late credit should repair loss%%, got %.0f%%", lp)
	}
}

// TestAdaptiveTimeout: the lost-timeout tracks the measured path RTT — it
// tightens to the floor on a fast path (so loss shows well before the -t
// ceiling) and rises to cover a genuinely slow hop (so honest replies aren't
// timed out early). A chronically-queued hop can't inflate it.
func TestAdaptiveTimeout(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(8, 8, 8, 8), 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4, opts: Options{Timeout: 2 * time.Second}}

	// No prompt replies yet → fall back to the ceiling (-t).
	if rto := s.rtoLocked(e.opts.Timeout); rto != e.opts.Timeout {
		t.Fatalf("empty-session RTO = %v, want ceiling %v", rto, e.opts.Timeout)
	}

	// Fast, stable path → RTO clamps to the floor, far under the 2s ceiling.
	h1 := s.getHop(1)
	for i := 0; i < 10; i++ {
		h1.updateRTO(40 * time.Millisecond)
	}
	if rto := s.rtoLocked(e.opts.Timeout); rto != rtoFloor {
		t.Fatalf("fast-path RTO = %v, want floor %v", rto, rtoFloor)
	}

	// The tightened timeout marks a 400ms-old probe lost, though -t is 2s.
	sm := seedPending(s, 1, 7)
	sm.sentAt = time.Now().Add(-400 * time.Millisecond)
	s.pending[7].sentAt = sm.sentAt
	e.sweep(time.Now())
	if sm.state != Lost {
		t.Fatalf("adaptive sweep should mark a 400ms probe lost at a 250ms RTO, state=%v", sm.state)
	}

	// A genuinely slow hop raises the path RTO to cover its honest ~600ms latency.
	h2 := s.getHop(2)
	for i := 0; i < 40; i++ {
		h2.updateRTO(600 * time.Millisecond)
	}
	rto := s.rtoLocked(e.opts.Timeout)
	if rto < 600*time.Millisecond || rto >= e.opts.Timeout {
		t.Fatalf("slow-path RTO = %v, want ≥600ms and under the 2s ceiling", rto)
	}

	// A chronically-queued hop (replies past queuedThreshold) never trains the
	// estimator, so it cannot drag the timeout toward its tens-of-seconds RTT.
	h3 := s.getHop(3)
	for i := 0; i < 40; i++ {
		h3.creditOK(40*time.Second, time.Now())
	}
	if got := s.rtoLocked(e.opts.Timeout); got != rto {
		t.Fatalf("queued hop inflated the RTO: %v, want unchanged %v", got, rto)
	}
}

// TestDeprioritizedFlag: a hop whose replies mostly arrive after the timeout
// (control-plane rate limiting) is flagged Deprioritized; a hop with equally
// high but *prompt* latency (a genuinely slow link) is not.
func TestDeprioritizedFlag(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)

	// Hop 1: 12 replies, 8 of them queued (≥ queuedThreshold) → ratio 8/12 > 0.5.
	deprio := s.getHop(1)
	deprio.addr = net.IPv4(10, 0, 0, 1)
	for i := 0; i < 8; i++ {
		deprio.noteOK(40*time.Second, time.Now())
	}
	for i := 0; i < 4; i++ {
		deprio.noteOK(40*time.Millisecond, time.Now())
	}

	// Hop 2: a genuinely slow-but-prompt link — high RTT, all under the threshold.
	slow := s.getHop(2)
	slow.addr = net.IPv4(10, 0, 0, 2)
	for i := 0; i < 12; i++ {
		slow.noteOK(600*time.Millisecond, time.Now())
	}

	// Hop 3: all queued but too few replies to judge yet.
	sparse := s.getHop(3)
	sparse.addr = net.IPv4(10, 0, 0, 3)
	for i := 0; i < 4; i++ {
		sparse.noteOK(40*time.Second, time.Now())
	}

	v := s.Snapshot(20)
	if !v.Hops[0].Deprioritized {
		t.Errorf("hop with majority-late replies should be flagged deprioritized")
	}
	if v.Hops[1].Deprioritized {
		t.Errorf("slow-but-prompt hop must not be flagged deprioritized")
	}
	if v.Hops[2].Deprioritized {
		t.Errorf("hop below the min-reply threshold must not be flagged yet")
	}
}

// TestStaleLowDestDoesNotCollapsePath: a transient reroute can briefly make the
// destination answer at a low TTL, stamping its address there. If that TTL's
// real router is silent, nothing overwrites the label — a plain smallest-TTL
// rule then collapses the path there forever and fakes an outage. Destination
// selection must prefer fresh evidence and clear the provably stale label.
func TestStaleLowDestDoesNotCollapsePath(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	now := time.Now()

	stuck := s.getHop(3)
	stuck.addr = target
	addOK(stuck, 1, 1600*time.Millisecond, now.Add(-5*time.Minute))

	// The destination really answers at TTL 8 (and, as always, above it).
	for ttl := 8; ttl <= 10; ttl++ {
		h := s.getHop(ttl)
		h.addr = target
		addOK(h, 2, 50*time.Millisecond, now)
	}

	v := s.Snapshot(10)
	if len(v.Hops) != 8 {
		t.Fatalf("stale low dest label collapsed the path: got %d hops, want 8", len(v.Hops))
	}
	if v.Hops[2].Addr != "" {
		t.Errorf("provably mislabeled hop 3 should have its address cleared, got %q", v.Hops[2].Addr)
	}
	if down, _ := s.destDownLocked(now); down {
		t.Errorf("destination answering at TTL 8 must not read as down")
	}
}

// TestDestStableThroughOutage: during a real outage every candidate's evidence
// is equally stale — the freshness filter must pass them all so the path and
// outage tracking stay stable instead of flapping.
func TestDestStableThroughOutage(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	now := time.Now()

	for ttl := 8; ttl <= 10; ttl++ {
		h := s.getHop(ttl)
		h.addr = target
		addOK(h, 1, 50*time.Millisecond, now.Add(-2*time.Minute))
	}

	if got := len(s.Snapshot(10).Hops); got != 8 {
		t.Fatalf("outage should not move the destination: got %d hops, want 8", got)
	}
	if down, _ := s.destDownLocked(now); !down {
		t.Errorf("2 minutes without a reply should read as down")
	}
}
