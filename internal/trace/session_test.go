package trace

import (
	"net"
	"testing"
	"time"
)

func addSamples(h *hop, startRound, n int) {
	base := time.Now().Add(-time.Duration(n) * time.Millisecond)
	for i := 0; i < n; i++ {
		addOK(h, startRound+i, time.Millisecond, base.Add(time.Duration(i)*time.Millisecond))
	}
}

// TestSnapshotKeepsFirstSampleAndRound verifies the window is not dropped/shifted
// (mtr shows the very first reply) and that each sample carries its round so the
// renderer can align glyphs across hops.
func TestSnapshotKeepsFirstSampleAndRound(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(1, 2, 3, 4), 30)
	h := s.getHop(1)
	addSamples(h, 1, 1) // a single reply on round 1 (odd)

	win := s.Snapshot(20).Hops[0].Samples
	if len(win) != 1 {
		t.Fatalf("first reply should be shown immediately, got %d samples", len(win))
	}
	if win[0].Round != 1 {
		t.Errorf("round not propagated: got %d, want 1", win[0].Round)
	}
}

func TestSnapshotTrimsToWindow(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(1, 2, 3, 4), 30)
	h := s.getHop(1)
	addSamples(h, 1, 100)

	win := s.Snapshot(20).Hops[0].Samples
	if len(win) != 20 {
		t.Fatalf("want 20 samples, got %d", len(win))
	}
	if win[len(win)-1].Round != 100 {
		t.Errorf("newest round should be 100, got %d", win[len(win)-1].Round)
	}
}

// TestMaxRTTTracksVisibleWindow is the regression for the stuck height scale: a
// huge spike that has scrolled out of the visible window must not keep the scale
// compressed.
func TestMaxRTTTracksVisibleWindow(t *testing.T) {
	s := newSession("IPv4", "x", net.IPv4(1, 2, 3, 4), 30)
	h := s.getHop(1)
	// One giant spike, then many small samples that push it out of the window.
	addOK(h, 1, 2*time.Second, time.Now())
	for i := 0; i < 50; i++ {
		addOK(h, 2+i, 40*time.Millisecond, time.Now())
	}

	if got := s.Snapshot(10).MaxRTT; got > 100*time.Millisecond {
		t.Errorf("scale still pinned to off-screen spike: MaxRTT=%v, want ~40ms", got)
	}
	// Worst (a cumulative stat) should still remember the spike.
	if w := s.Snapshot(10).Hops[0].Worst; w < time.Second {
		t.Errorf("Worst should retain the spike across full history, got %v", w)
	}
}

func TestIsBogusPTR(t *testing.T) {
	bogus := []string{
		"",
		"undefined.hostname.localhost",
		"localhost",
		"router",          // no dot
		"foo.unknown.net", // contains "unknown"
		"1.0.0.127.in-addr.arpa",
	}
	for _, n := range bogus {
		if !isBogusPTR(n) {
			t.Errorf("expected %q to be bogus", n)
		}
	}
	good := []string{"host.starlinkisp.net", "pnseab-ac-in-f14.1e100.net", "dns.google"}
	for _, n := range good {
		if isBogusPTR(n) {
			t.Errorf("expected %q to be accepted", n)
		}
	}
}
