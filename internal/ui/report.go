package ui

import (
	"fmt"
	"strings"
	"time"

	"tracer/internal/sparkline"
	"tracer/internal/trace"
)

// Report renders a plain-text (no ANSI, no alt-screen) snapshot of the sessions,
// suitable for piping or logging. The sparkline is monochrome braille.
func Report(sessions []*trace.Session) string {
	const sparkW = 40
	views := make([]trace.SessionView, len(sessions))
	for i, s := range sessions {
		views[i] = s.Snapshot(sparkW * 2)
	}

	showASN := false
	for _, v := range views {
		for _, h := range v.Hops {
			if h.ASN != "" {
				showASN = true
			}
		}
	}

	var b strings.Builder
	for _, v := range views {
		ceiling := v.MaxRTT // per-family scale, like the TUI
		if ceiling <= 0 {
			ceiling = time.Millisecond
		}
		fmt.Fprintf(&b, "%s → %s (%s)  bars 0–%s\n", v.Family, v.Target, v.TargetIP, formatMS(ceiling))
		if showASN {
			fmt.Fprintf(&b, "%3s  %-30s %-8s %5s %8s %8s %8s %8s %8s  %s\n",
				"hop", "host", "asn", "loss", "last", "avg", "best", "wrst", "jitter", "history")
		} else {
			fmt.Fprintf(&b, "%3s  %-30s %5s %8s %8s %8s %8s %8s  %s\n",
				"hop", "host", "loss", "last", "avg", "best", "wrst", "jitter", "history")
		}
		for _, h := range v.Hops {
			host := h.Host
			if host == "" {
				host = "(waiting)"
			}
			loss := fmt.Sprintf("%4.0f%%", h.LossPct)
			if h.Sent == 0 {
				loss = "   --"
			}
			if showASN {
				fmt.Fprintf(&b, "%2d.  %-30s %-8s %5s %8s %8s %8s %8s %8s  %s\n",
					h.TTL, truncate(host, 30), truncate(h.ASN, 8), loss,
					dur(h.Last), dur(h.Avg), dur(h.Best), dur(h.Worst), dur(h.Jitter),
					plainSpark(h.Samples, ceiling))
			} else {
				fmt.Fprintf(&b, "%2d.  %-30s %5s %8s %8s %8s %8s %8s  %s\n",
					h.TTL, truncate(host, 30), loss,
					dur(h.Last), dur(h.Avg), dur(h.Best), dur(h.Worst), dur(h.Jitter),
					plainSpark(h.Samples, ceiling))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("⣀ low → ⣿ high (per family)   ⣿ = loss\n")
	return b.String()
}

func plainSpark(samples []trace.SampleView, ceiling time.Duration) string {
	points := make([]sparkline.Point, len(samples))
	for i, s := range samples {
		switch s.State {
		case trace.OK:
			points[i] = sparkline.Point{Filled: true, Height: heightFor(s.RTT, ceiling)}
		case trace.Lost:
			points[i] = sparkline.Point{Filled: true, Loss: true}
		}
	}
	return sparkline.String(sparkline.Cells(points))
}

func dur(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	return formatMS(d)
}
