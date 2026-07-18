package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"tracer/internal/trace"
)

// sampleSeq builds OK/Lost samples on consecutive rounds starting at 1.
func sampleSeq(rtts ...float64) []trace.SampleView {
	out := make([]trace.SampleView, len(rtts))
	for i, ms := range rtts {
		s := trace.SampleView{Round: i + 1}
		if ms < 0 {
			s.State = trace.Lost
		} else {
			s.State = trace.OK
			s.RTT = time.Duration(ms * float64(time.Millisecond))
		}
		out[i] = s
	}
	return out
}

func testViews() []trace.SessionView {
	return []trace.SessionView{
		{
			Family: "IPv4", Target: "google.com", TargetIP: "142.250.72.46", DestFound: true,
			MaxRTT: 72 * time.Millisecond,
			Hops: []trace.HopView{
				{TTL: 1, Host: "gateway", Addr: "192.168.1.1", Recv: 6, Last: 3 * time.Millisecond, Avg: 3 * time.Millisecond, Samples: sampleSeq(3, 3, 3, 4, 3, 3)},
				{TTL: 2, Host: "(waiting for reply)", LossPct: 100, Sent: 6, Samples: sampleSeq(-1, -1, -1, -1, -1, -1)},
				{TTL: 3, Host: "dns.google", Addr: "8.8.8.8", Recv: 6, Last: 70 * time.Millisecond, Avg: 68 * time.Millisecond, Samples: sampleSeq(70, 65, 72, 68, 70, 71)},
			},
		},
	}
}

func TestRenderFrameContainsHopsAndBraille(t *testing.T) {
	now := time.Date(2026, 6, 25, 22, 47, 30, 0, time.UTC)
	out := renderFrame(testViews(), 120, "kaiton.local", 40, now, true, false)

	for _, want := range []string{"tracer", "IPv4 → google.com (142.250.72.46)", "gateway", "dns.google", "bars 0–72.0ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q", want)
		}
	}
	if !strings.ContainsAny(out, "⣀⣤⣶⣿⡀⢀⠀") {
		t.Errorf("frame has no braille sparkline characters")
	}
}

func TestSilentHopsCollapse(t *testing.T) {
	views := []trace.SessionView{{
		Family: "IPv4", Target: "x", TargetIP: "1.2.3.4", DestFound: true,
		Hops: []trace.HopView{
			{TTL: 1, Host: "gw", Addr: "192.168.1.1", Recv: 6, Last: time.Millisecond, Samples: sampleSeq(3, 3)},
			{TTL: 2, LossPct: 100, Sent: 6}, // silent
			{TTL: 3, LossPct: 100, Sent: 6}, // silent, consecutive
			{TTL: 4, Host: "dst", Addr: "1.2.3.4", Recv: 6, Last: time.Millisecond, Samples: sampleSeq(5, 5)},
		},
	}}
	now := time.Date(2026, 6, 25, 22, 47, 30, 0, time.UTC)
	out := renderFrame(views, 120, "h", 40, now, true, false)

	if !strings.Contains(out, "2-3 (no reply)") {
		t.Errorf("consecutive silent hops not collapsed into a range; got:\n%s", out)
	}
	if strings.Count(out, "(no reply)") != 1 {
		t.Errorf("expected exactly one collapsed silent line, got %d", strings.Count(out, "(no reply)"))
	}
}

// TestRenderSparkAlignsByRound checks that two hops sharing the latest round land
// it in the same (rightmost) column, even if one hop was discovered later and has
// fewer samples — the core of the cross-hop alignment.
func TestRenderSparkAlignsByRound(t *testing.T) {
	ceiling := 10 * time.Millisecond
	full := sampleSeq(5, 5, 5, 5)                                                      // rounds 1..4
	late := []trace.SampleView{{State: trace.OK, RTT: 5 * time.Millisecond, Round: 3}, // discovered at round 3
		{State: trace.OK, RTT: 5 * time.Millisecond, Round: 4}}

	const sparkW, maxRound = 6, 4
	a := []rune(renderSpark(full, ceiling, sparkW, maxRound, false))
	b := []rune(renderSpark(late, ceiling, sparkW, maxRound, false))

	if len(a) != sparkW || len(b) != sparkW {
		t.Fatalf("sparkline width: got %d and %d, want %d", len(a), len(b), sparkW)
	}
	if a[len(a)-1] != b[len(b)-1] {
		t.Errorf("newest round not aligned to the same rightmost glyph: %q vs %q", string(a[len(a)-1]), string(b[len(b)-1]))
	}
}

func TestConnectivityBanner(t *testing.T) {
	mk := func(family string, destFound bool, sinceOK time.Duration, samples []trace.SampleView) trace.SessionView {
		return trace.SessionView{
			Family: family, Target: "google.com", TargetIP: "x", DestFound: destFound,
			Hops: []trace.HopView{{TTL: 1, Host: "dst", Addr: "8.8.8.8", Recv: 1, SinceOK: sinceOK, Last: 40 * time.Millisecond, Samples: samples}},
		}
	}
	ok := sampleSeq(40, 40, 40, 40)
	lost := sampleSeq(-1, -1, -1, -1)

	cases := []struct {
		name string
		v    []trace.SessionView
		want string
	}{
		{"online", []trace.SessionView{mk("IPv4", true, 100*time.Millisecond, ok)}, "ONLINE"},
		{"offline", []trace.SessionView{mk("IPv4", true, 5*time.Second, lost)}, "OFFLINE"},
		{"connecting", []trace.SessionView{mk("IPv4", false, -1, nil)}, "…"},
		// v4 down but v6 up → still ONLINE (Zoom works on either family)
		{"mixed online", []trace.SessionView{mk("IPv4", true, 5*time.Second, lost), mk("IPv6", true, 80*time.Millisecond, ok)}, "ONLINE"},
	}
	for _, c := range cases {
		if got := renderStatus(c.v); !strings.Contains(got, c.want) {
			t.Errorf("%s: banner %q missing %q", c.name, got, c.want)
		}
	}
}

func TestRenderFrameRowWidthBounded(t *testing.T) {
	now := time.Date(2026, 6, 25, 22, 47, 30, 0, time.UTC)
	const width = 100
	out := renderFrame(testViews(), width, "kaiton.local", 40, now, true, false)
	for _, line := range strings.Split(out, "\n") {
		if w := visibleWidth(line); w > width {
			t.Errorf("line exceeds width %d (got %d): %q", width, w, line)
		}
	}
}

// visibleWidth measures display cells, ignoring ANSI escape sequences.
func visibleWidth(s string) int {
	return lipgloss.Width(s)
}

func TestPeriodicity(t *testing.T) {
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	at := func(secs ...int) []time.Time {
		out := make([]time.Time, len(secs))
		for i, s := range secs {
			out[i] = base.Add(time.Duration(s) * time.Second)
		}
		return out
	}

	if d, ok := periodicity(at(0, 15, 31, 44, 60)); !ok || d < 13*time.Second || d > 17*time.Second {
		t.Errorf("regular ~15s cadence: ok=%v d=%v", ok, d)
	}
	if _, ok := periodicity(at(0, 3, 40, 41, 95)); ok {
		t.Errorf("irregular intervals should not be called periodic")
	}
	if _, ok := periodicity(at(0, 15)); ok {
		t.Errorf("too few outages should not be called periodic")
	}
}
