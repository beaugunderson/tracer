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
