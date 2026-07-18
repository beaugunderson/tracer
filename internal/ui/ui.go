// Package ui renders live traceroute sessions as a continuously refreshing TUI.
package ui

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tracer/internal/sparkline"
	"tracer/internal/trace"
)

const (
	colIdx    = 3  // "10."
	colHost   = 30 // hostname / address column
	colASN    = 8  // AS name, e.g. "GOOGLE"/"STARLINK", ellipsized (only when -z)
	colLoss   = 5  // " 100%"
	colLast   = 6  // "47.0ms"
	colJitter = 7  // "±8.2ms"
	colGutter = 2
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	lossStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	waitStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

// gradientPalette is a cool→warm 256-color ramp for the per-row latency color.
// It deliberately stops short of red, which is reserved for packet loss.
var gradientPalette = []string{"27", "39", "45", "48", "154", "220", "208"}

var gradientStyles = func() []lipgloss.Style {
	s := make([]lipgloss.Style, len(gradientPalette))
	for i, c := range gradientPalette {
		s[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(c))
	}
	return s
}()

// colorFloor is the minimum per-row latency span used as the gradient denominator
// so a hop with only sub-millisecond jitter doesn't get stretched to full color.
const colorFloor = 3 * time.Millisecond

// glyph style keys for run-length rendering.
const (
	keyPlain = -2
	keyLoss  = -1
)

// downThreshold is how long without a reply from the destination before a family
// is called OFFLINE. Shared with the engine's outage tracking.
const downThreshold = trace.DownThreshold

var (
	upStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	degStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	downStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("9"))
)

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Model is the bubbletea model wrapping the live sessions.
type Model struct {
	sessions []*trace.Session
	hostname string
	color    bool // per-row latency color gradient
	asn      bool // show an ASN column
	width    int
	height   int
}

// New builds a Model over the given sessions. color enables the per-row gradient;
// asn shows the origin-AS column.
func New(sessions []*trace.Session, color, asn bool) Model {
	host, _ := os.Hostname()
	return Model{sessions: sessions, hostname: host, color: color, asn: asn, width: 80, height: 24}
}

func (m Model) Init() tea.Cmd { return tick() }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Force a full clear+repaint: bubbletea's line-diff renderer leaves
		// stale rows behind on resize (especially after the tab was backgrounded
		// and a resize was missed).
		return m, tea.ClearScreen
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			for _, s := range m.sessions {
				s.Reset()
			}
		}
	case tickMsg:
		return m, tick()
	}
	return m, nil
}

func (m Model) View() string {
	sparkW := sparkWidth(m.width, m.asn)
	maxSamples := sparkW * 2

	views := make([]trace.SessionView, len(m.sessions))
	for i, s := range m.sessions {
		views[i] = s.Snapshot(maxSamples)
	}
	frame := renderFrame(views, m.width, m.hostname, maxSamples, time.Now(), m.color, m.asn)

	// Pad to the viewport height so a frame that shrank (a hop collapsing, the
	// path clamping) overwrites the taller previous frame's trailing lines
	// instead of leaving them as residue.
	if lines := strings.Count(frame, "\n") + 1; lines < m.height {
		frame += strings.Repeat("\n", m.height-lines)
	}
	return frame
}

func sparkWidth(width int, asn bool) int {
	prefix := colIdx + 1 + colHost + 1 + colLoss + 1 + colLast + 1 + colJitter + colGutter
	if asn {
		prefix += colASN + 1
	}
	w := width - prefix
	if w < 4 {
		return 4
	}
	return w
}

// renderFrame is the pure TUI renderer: given snapshots it produces the full
// screen string. Kept free of Model/clock state so it can be tested directly.
func renderFrame(views []trace.SessionView, width int, hostname string, samples int, now time.Time, color, asn bool) string {
	sparkW := sparkWidth(width, asn)

	var b strings.Builder
	b.WriteString(renderTitle(width, hostname, samples, now))
	b.WriteString("\n")
	b.WriteString(renderStatus(views))
	b.WriteString("\n\n")
	for i := range views {
		// Each family scales its bars to its own worst hop (over the visible
		// window), so a slow family doesn't flatten a fast one and vice versa.
		ceiling := views[i].MaxRTT
		if ceiling <= 0 {
			ceiling = time.Millisecond
		}
		b.WriteString(renderSession(views[i], ceiling, sparkW, color, asn))
		if i != len(views)-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	if og := renderOutages(views, now); og != "" {
		b.WriteString(og)
		b.WriteString("\n\n") // blank line between the outage list and the legend
	}
	b.WriteString(renderFooter(color, width))
	return b.String()
}

type outageEntry struct {
	family string
	o      trace.Outage
}

// renderOutages lists recent destination-unreachable episodes (most recent first)
// and, when a family's outages recur at a regular interval, notes the period.
func renderOutages(views []trace.SessionView, now time.Time) string {
	var entries []outageEntry
	periods := map[string]time.Duration{}
	for _, v := range views {
		fam := shortFamily(v.Family)
		var starts []time.Time
		for _, o := range v.Outages {
			entries = append(entries, outageEntry{fam, o})
			starts = append(starts, o.Start)
		}
		if p, ok := periodicity(starts); ok {
			periods[fam] = p
		}
	}
	if len(entries) == 0 {
		return ""
	}
	// Most recent first (ongoing outages sort to the top via their start time).
	sort.Slice(entries, func(i, j int) bool { return entries[i].o.Start.After(entries[j].o.Start) })

	head := titleStyle.Render("recent outages")
	if len(periods) > 0 {
		var hints []string
		for _, v := range views { // stable family order
			fam := shortFamily(v.Family)
			if p, ok := periods[fam]; ok {
				hints = append(hints, fmt.Sprintf("%s ≈ every %s", fam, shortDur(p)))
			}
		}
		head += dimStyle.Render("   " + strings.Join(hints, " · "))
	}

	var b strings.Builder
	b.WriteString(head)
	const maxShown = 6
	for i, e := range entries {
		if i >= maxShown {
			b.WriteString(dimStyle.Render(fmt.Sprintf("\n  …and %d more", len(entries)-maxShown)))
			break
		}
		dur := shortDur(e.o.Duration(now))
		when := shortDur(now.Sub(e.o.End)) + " ago"
		if e.o.Ongoing() {
			when = "ongoing"
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("\n  %-3s %5s   %s", e.family, dur, when)))
	}
	return b.String()
}

// periodicity returns the mean interval between outage onsets when they recur
// regularly (low spread), so we don't claim a pattern that isn't there.
func periodicity(starts []time.Time) (time.Duration, bool) {
	if len(starts) < 4 {
		return 0, false
	}
	sorted := append([]time.Time(nil), starts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	var sum, sumSq float64
	n := float64(len(sorted) - 1)
	for i := 1; i < len(sorted); i++ {
		d := sorted[i].Sub(sorted[i-1]).Seconds()
		sum += d
		sumSq += d * d
	}
	mean := sum / n
	if mean <= 0 {
		return 0, false
	}
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	if math.Sqrt(variance)/mean > 0.4 { // too irregular to call periodic
		return 0, false
	}
	return time.Duration(mean * float64(time.Second)), true
}

func shortFamily(family string) string { return strings.TrimPrefix(family, "IP") }

func renderTitle(width int, hostname string, samples int, now time.Time) string {
	left := titleStyle.Render("tracer")
	ts := now.Format("2006-01-02T15:04:05-0700")
	mid := fmt.Sprintf("%s  ·  last %d pings", hostname, samples)
	right := dimStyle.Render(ts)
	line := fmt.Sprintf("%s  %s", left, mid)
	pad := width - lipgloss.Width(line) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return line + strings.Repeat(" ", pad) + right
}

type connState int

const (
	stConnecting connState = iota
	stUp
	stDegraded
	stDown
)

type famStatus struct {
	label   string
	state   connState
	rtt     time.Duration
	downFor time.Duration
	lossPct float64
}

// familyStatus derives end-to-end reachability for one family from how long ago
// the destination last replied (responsive — it doesn't wait on the loss sweep).
func familyStatus(v trace.SessionView) famStatus {
	fs := famStatus{label: strings.TrimPrefix(v.Family, "IP")} // "IPv4" → "v4"
	if !v.DestFound || len(v.Hops) == 0 {
		return fs // stConnecting
	}
	dest := v.Hops[len(v.Hops)-1]
	if dest.SinceOK < 0 {
		return fs // never replied yet
	}
	if dest.SinceOK >= downThreshold {
		fs.state, fs.downFor = stDown, dest.SinceOK
		return fs
	}
	if loss := recentLoss(dest.Samples, 8); loss > 0.2 {
		fs.state, fs.lossPct, fs.rtt = stDegraded, loss*100, dest.Recent
		return fs
	}
	fs.state, fs.rtt = stUp, dest.Recent
	return fs
}

// recentLoss is the lost fraction over the last n decided (non-pending) samples.
func recentLoss(samples []trace.SampleView, n int) float64 {
	ok, lost := 0, 0
	for i := len(samples) - 1; i >= 0 && ok+lost < n; i-- {
		switch samples[i].State {
		case trace.OK:
			ok++
		case trace.Lost:
			lost++
		}
	}
	if ok+lost == 0 {
		return 0
	}
	return float64(lost) / float64(ok+lost)
}

// renderStatus is the glanceable connectivity banner. Any family up → ONLINE;
// all families down → OFFLINE (the meeting-killer); otherwise UNSTABLE.
func renderStatus(views []trace.SessionView) string {
	if len(views) == 0 {
		return ""
	}
	fss := make([]famStatus, len(views))
	anyUp, allDown := false, true
	var offlineFor time.Duration = -1
	for i, v := range views {
		fss[i] = familyStatus(v)
		switch fss[i].state {
		case stUp:
			anyUp = true
			allDown = false
		case stDown:
			if offlineFor < 0 || fss[i].downFor < offlineFor {
				offlineFor = fss[i].downFor // shortest = how long fully cut off
			}
		default:
			allDown = false
		}
	}

	var badge string
	switch {
	case anyUp:
		badge = upStyle.Render(" ● ONLINE ")
	case allDown:
		badge = downStyle.Render(fmt.Sprintf(" ● OFFLINE %s ", shortDur(offlineFor)))
	default:
		badge = degStyle.Render(" ◐ UNSTABLE ")
	}

	parts := make([]string, len(fss))
	for i, fs := range fss {
		parts[i] = fs.label + " " + famDetail(fs)
	}
	return badge + "  " + dimStyle.Render(strings.Join(parts, " · "))
}

func famDetail(fs famStatus) string {
	switch fs.state {
	case stUp:
		return formatMS(fs.rtt)
	case stDegraded:
		return fmt.Sprintf("%.0f%% loss", fs.lossPct)
	case stDown:
		return "down " + shortDur(fs.downFor)
	default:
		return "…"
	}
}

func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

func renderSession(v trace.SessionView, ceiling time.Duration, sparkW int, color, asn bool) string {
	// Each family aligns its right edge to its OWN newest round; the two families
	// have independent, drifting round counters, so a shared edge would leave a
	// gap on whichever is a round or two behind.
	maxRound := 0
	for _, h := range v.Hops {
		if n := len(h.Samples); n > 0 && h.Samples[n-1].Round > maxRound {
			maxRound = h.Samples[n-1].Round
		}
	}

	var b strings.Builder
	dest := v.TargetIP
	if !v.DestFound {
		dest += " (resolving path…)"
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("%s → %s (%s)", v.Family, v.Target, dest)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("   bars 0–%s", formatMS(ceiling))))
	if v.DestFound && len(v.Hops) > 0 {
		// Availability = how often the destination answered (recent history).
		b.WriteString(dimStyle.Render(fmt.Sprintf("   %.1f%% up", 100-v.Hops[len(v.Hops)-1].LossPct)))
	}
	b.WriteString("\n")

	// Collapse runs of hops that have never answered (filtered routers, mtr's
	// red "???") into a single dim summary line instead of a wall of red.
	for i := 0; i < len(v.Hops); {
		if isSilent(v.Hops[i]) {
			j := i
			for j < len(v.Hops) && isSilent(v.Hops[j]) {
				j++
			}
			b.WriteString(renderSilent(v.Hops[i].TTL, v.Hops[j-1].TTL))
			b.WriteString("\n")
			i = j
			continue
		}
		b.WriteString(renderHop(v.Hops[i], ceiling, sparkW, maxRound, color, asn))
		b.WriteString("\n")
		i++
	}
	return b.String()
}

// isSilent reports a hop that has been probed but never returned a reply and has
// no address — a non-responding router rather than measured loss on a live hop.
func isSilent(h trace.HopView) bool {
	return h.Addr == "" && h.Recv == 0
}

func renderSilent(start, end int) string {
	label := fmt.Sprintf("%d", start)
	if end != start {
		label = fmt.Sprintf("%d-%d", start, end)
	}
	return dimStyle.Render(fmt.Sprintf("%3s %s", label, "(no reply)"))
}

func renderHop(h trace.HopView, ceiling time.Duration, sparkW, maxRound int, color, asn bool) string {
	idx := fmt.Sprintf("%2d.", h.TTL)

	host := h.Host
	if host == "" {
		host = waitStyle.Render("(waiting for reply)")
	}
	host = truncate(host, colHost)
	hostCell := host + strings.Repeat(" ", max0(colHost-lipgloss.Width(host)))

	loss := fmt.Sprintf("%4.0f%%", h.LossPct)
	if h.Sent == 0 {
		loss = "   --"
	}
	last := "    --"
	if h.Recent > 0 {
		last = fmt.Sprintf("%6s", formatMS(h.Recent))
	}
	jitter := fmt.Sprintf("%7s", "--")
	if h.Jitter > 0 {
		jitter = fmt.Sprintf("%7s", "±"+formatMS(h.Jitter))
	}

	spark := renderSpark(h.Samples, ceiling, sparkW, maxRound, color)

	cols := []string{dimStyle.Render(idx), hostCell}
	if asn {
		a := truncate(h.ASN, colASN)
		cols = append(cols, dimStyle.Render(a+strings.Repeat(" ", max0(colASN-lipgloss.Width(a)))))
	}
	cols = append(cols, dimStyle.Render(loss), last, dimStyle.Render(jitter))
	return strings.Join(cols, " ") + strings.Repeat(" ", colGutter) + spark
}

// renderSpark draws the latest sparkW glyphs ending at the global newest round.
// Each ping is placed into glyph round/2 (even round → left dot, odd → right
// dot), so a ping always occupies the same column (no jitter), every hop aligns
// to the same columns regardless of when it was discovered, and the first reply
// shows immediately. Rounds with no sample render blank, so a hop missing the
// latest round shows a blank right edge until its reply lands — live and aligned.
func renderSpark(samples []trace.SampleView, ceiling time.Duration, sparkW, maxRound int, color bool) string {
	points := make(map[int]sparkline.Point, len(samples))
	rtts := make(map[int]time.Duration, len(samples))
	var rowMin, rowMax time.Duration
	have := false
	for _, s := range samples {
		points[s.Round] = pointFor(s, ceiling)
		if s.State == trace.OK {
			rtts[s.Round] = s.RTT
			if !have || s.RTT < rowMin {
				rowMin = s.RTT
			}
			if !have || s.RTT > rowMax {
				rowMax = s.RTT
			}
			have = true
		}
	}

	maxGlyph := maxRound / 2
	runes := make([]rune, 0, sparkW)
	keys := make([]int, 0, sparkW)
	for g := maxGlyph - sparkW + 1; g <= maxGlyph; g++ {
		if g < 0 {
			runes = append(runes, ' ') // before any history: blank left padding
			keys = append(keys, keyPlain)
			continue
		}
		cell := sparkline.Glyph(points[2*g], points[2*g+1]) // even round left, odd right
		key := keyPlain
		switch {
		case cell.Loss:
			key = keyLoss
		case color && have:
			// Color the glyph by the worse of its two pings, on this row's scale.
			if w := maxDur(rtts[2*g], rtts[2*g+1]); w > 0 {
				key = colorIndexFor(w, rowMin, rowMax)
			}
		}
		runes = append(runes, cell.R)
		keys = append(keys, key)
	}
	return renderRuns(runes, keys)
}

// renderRuns emits the glyphs, applying a style per run of equal keys so a
// gradient row costs a handful of ANSI escapes instead of one per cell.
func renderRuns(runes []rune, keys []int) string {
	var b strings.Builder
	for i := 0; i < len(runes); {
		j := i + 1
		for j < len(runes) && keys[j] == keys[i] {
			j++
		}
		seg := string(runes[i:j])
		switch k := keys[i]; {
		case k == keyLoss:
			b.WriteString(lossStyle.Render(seg))
		case k >= 0:
			b.WriteString(gradientStyles[k].Render(seg))
		default:
			b.WriteString(seg)
		}
		i = j
	}
	return b.String()
}

// colorIndexFor maps an RTT to a gradient index against this row's own span.
func colorIndexFor(rtt, rowMin, rowMax time.Duration) int {
	denom := rowMax - rowMin
	if denom < colorFloor {
		denom = colorFloor
	}
	norm := float64(rtt-rowMin) / float64(denom)
	if norm < 0 {
		norm = 0
	}
	if norm > 1 {
		norm = 1
	}
	return int(math.Round(norm * float64(len(gradientPalette)-1)))
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func pointFor(s trace.SampleView, ceiling time.Duration) sparkline.Point {
	switch s.State {
	case trace.OK:
		return sparkline.Point{Filled: true, Height: heightFor(s.RTT, ceiling)}
	case trace.Lost:
		return sparkline.Point{Filled: true, Loss: true}
	default: // Pending or absent renders blank
		return sparkline.Point{}
	}
}

func renderFooter(color bool, width int) string {
	bars := titleStyle.Render("bars") + " " + dimStyle.Render("⣀ low → ⣿ high (per family)")
	loss := lossStyle.Render("⣿ = loss")
	keys := dimStyle.Render("q quit · r reset stats")
	essential := fmt.Sprintf("%s   %s   %s", bars, loss, keys)

	if !color {
		return essential
	}
	// The per-row color ramp is the most verbose part; include it only if the
	// whole footer still fits the width, otherwise keep the essentials.
	ramp := dimStyle.Render("color = each hop's own min ")
	for i := range gradientStyles {
		ramp += gradientStyles[i].Render("⣿")
	}
	ramp += dimStyle.Render(" max")
	full := fmt.Sprintf("%s   %s   %s   %s", bars, ramp, loss, keys)
	if lipgloss.Width(full) <= width {
		return full
	}
	return essential
}

func heightFor(rtt, ceiling time.Duration) int {
	if ceiling <= 0 {
		return 1
	}
	h := int(math.Round(float64(rtt) / float64(ceiling) * 4))
	if h < 1 {
		return 1 // a received probe always shows at least one dot
	}
	if h > 4 {
		return 4
	}
	return h
}

func formatMS(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	switch {
	case ms >= 100:
		return fmt.Sprintf("%.0fms", ms)
	case ms >= 10:
		return fmt.Sprintf("%.1fms", ms)
	default:
		return fmt.Sprintf("%.2fms", ms)
	}
}

func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if w <= 1 {
		return string(r[:max0(w)])
	}
	return string(r[:w-1]) + "…"
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
