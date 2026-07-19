package trace

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// quotedV4Echo builds the packet an ICMP error quotes back: a 20-byte IPv4
// header followed by the first 8 bytes of our echo request (carrying id + seq).
func quotedV4Echo(id, seq uint16) []byte {
	data := make([]byte, 28)
	data[0] = 0x45 // IPv4, IHL=5 → 20-byte header
	data[20] = 8   // inner ICMP type: echo request
	binary.BigEndian.PutUint16(data[24:26], id)
	binary.BigEndian.PutUint16(data[26:28], seq)
	return data
}

func seedPending(s *Session, ttl int, seq uint16) *sample {
	sm := &sample{seq: seq, sentAt: time.Now(), state: Pending}
	h := s.getHop(ttl)
	h.samples = append(h.samples, sm)
	s.pending[seq] = &pendingProbe{ttl: ttl, s: sm, sentAt: sm.sentAt}
	return sm
}

// TestDstUnreachFromIntermediateIgnored is the regression for vanishing hops: a
// Destination Unreachable from an intermediate router (e.g. a Starlink dropout)
// must not be recorded as that hop reaching the target.
func TestDstUnreachFromIntermediateIgnored(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4}

	sm := seedPending(s, 5, 7)
	msg := &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Body: &icmp.DstUnreach{Data: quotedV4Echo(1234, 7)}}

	// From an intermediate router: ignored — probe stays pending, no address set.
	e.handle(msg, &net.IPAddr{IP: net.IPv4(10, 0, 0, 1)})
	if sm.state != Pending {
		t.Fatalf("intermediate unreachable recorded a reply (state=%v)", sm.state)
	}
	if s.hops[5].addr != nil {
		t.Fatalf("intermediate unreachable set hop address %v", s.hops[5].addr)
	}

	// From the target itself: that genuinely reached the destination.
	e.handle(msg, &net.IPAddr{IP: target})
	if a := s.hops[5].addr; a == nil || !a.Equal(target) {
		t.Fatalf("unreachable from target should set hop addr to target, got %v", a)
	}
	if !s.Snapshot(10).DestFound {
		t.Fatalf("a hop answering as the target should make DestFound true")
	}
}

// TestPathLengthTracksAddresses verifies the displayed path follows route changes:
// the destination is the smallest TTL whose address is the target, recomputed live.
func TestPathLengthTracksAddresses(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)

	// Intermediates at 1-2; the target answers at 3 and (as during discovery) also
	// at the higher TTLs 4-5 that echo-reply.
	s.getHop(1).addr = net.IPv4(10, 0, 0, 1)
	s.getHop(2).addr = net.IPv4(10, 0, 0, 2)
	for ttl := 3; ttl <= 5; ttl++ {
		s.getHop(ttl).addr = target
	}

	v := s.Snapshot(10)
	if !v.DestFound || len(v.Hops) != 3 {
		t.Fatalf("want path capped at TTL 3, got DestFound=%v len=%d", v.DestFound, len(v.Hops))
	}

	// Route lengthens: TTLs 3-5 are now intermediates, the target answers at 6.
	s.getHop(3).addr = net.IPv4(10, 0, 0, 3)
	s.getHop(4).addr = net.IPv4(10, 0, 0, 4)
	s.getHop(5).addr = net.IPv4(10, 0, 0, 5)
	s.getHop(6).addr = target
	if l := len(s.Snapshot(10).Hops); l != 6 {
		t.Fatalf("path should grow to 6 hops after reroute, got %d", l)
	}
}

// addOK appends a received sample and updates the hop's running aggregates,
// mirroring what recordReply does in production.
func addOK(h *hop, round int, rtt time.Duration, sentAt time.Time) {
	h.samples = append(h.samples, &sample{state: OK, round: round, rtt: rtt, sentAt: sentAt})
	if len(h.samples) > sampleCap {
		h.samples = h.samples[len(h.samples)-sampleCap:]
	}
	h.noteOK(rtt, sentAt)
}

func TestJitterIsStdDev(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(8, 8, 8, 8), 30)
	h := s.getHop(1)
	for i, ms := range []int{10, 20, 30, 40, 50} {
		addOK(h, i+1, time.Duration(ms)*time.Millisecond, time.Now())
	}
	// mean 30ms, population stddev = sqrt(200) ≈ 14.14ms.
	got := s.Snapshot(10).Hops[0].Jitter
	want := 14140 * time.Microsecond
	if d := got - want; d < -300*time.Microsecond || d > 300*time.Microsecond {
		t.Errorf("jitter = %v, want ≈ %v", got, want)
	}
}

func TestRecentSmoothsLastPings(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(8, 8, 8, 8), 30)
	h := s.getHop(1)
	// Steady 20ms with a single 80ms spike as the newest reply.
	base := time.Now()
	for i, ms := range []int{20, 20, 20, 20, 20, 80} {
		addOK(h, i+1, time.Duration(ms)*time.Millisecond, base.Add(time.Duration(i)*time.Second))
	}
	hv := s.Snapshot(10).Hops[0]
	if hv.Last != 80*time.Millisecond {
		t.Errorf("Last should be the raw newest ping 80ms, got %v", hv.Last)
	}
	// Mean of last 5 = (20+20+20+20+80)/5 = 32ms — smoothed, not the 80ms spike.
	if hv.Recent != 32*time.Millisecond {
		t.Errorf("Recent should average the last 5 to 32ms, got %v", hv.Recent)
	}
}

func TestOutageTracking(t *testing.T) {
	target := net.IPv4(8, 8, 8, 8)
	s := newSession("IPv4", "x", target, 30)
	h := s.getHop(1)
	h.addr = target

	now := time.Now()
	// Last reply was 3s ago → past the down threshold → an outage opens.
	addOK(h, 1, time.Millisecond, now.Add(-3*time.Second))
	s.updateOutagesLocked(now)
	if len(s.outages) != 1 || !s.outageOpen {
		t.Fatalf("expected one open outage, got %d open=%v", len(s.outages), s.outageOpen)
	}

	// A fresh reply → the outage closes.
	addOK(h, 2, time.Millisecond, now)
	s.updateOutagesLocked(now)
	if s.outageOpen || s.outages[0].End.IsZero() {
		t.Fatalf("expected outage closed with an End set, open=%v end=%v", s.outageOpen, s.outages[0].End)
	}
}

// TestAddrChangeClearsStaleLabels guards the wrong-ASN-on-CGNAT bug: when a hop's
// address changes, its stale host/ASN label must be dropped.
func TestAddrChangeClearsStaleLabels(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(8, 8, 8, 8), 30)
	e := &engine{s: s, id: 1234, proto: protoICMP4}

	seedPending(s, 3, 1)
	e.recordReply(1, &net.IPAddr{IP: net.IPv4(206, 224, 66, 1)})
	s.mu.Lock()
	s.hops[3].host = "host.starlinkisp.net"
	s.hops[3].asn = "STARLINK"
	s.mu.Unlock()

	// The hop's address flips to the CGNAT.
	seedPending(s, 3, 2)
	e.recordReply(2, &net.IPAddr{IP: net.IPv4(100, 64, 0, 1)})

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hops[3].host != "" || s.hops[3].asn != "" {
		t.Errorf("stale labels retained after address change: host=%q asn=%q", s.hops[3].host, s.hops[3].asn)
	}
}
