// Package sparkline renders latency history as Unicode braille sparklines.
//
// Each braille glyph (U+2800..U+28FF) is a 2-wide by 4-tall dot matrix, so one
// character encodes two consecutive samples as vertical bars 0..4 dots tall.
// A row of N glyphs therefore shows 2N samples in the horizontal space of N
// columns, denser than one-sample-per-column block sparklines.
package sparkline

// Dot bit layout within a braille cell (Unicode numbering):
//
//	1 4
//	2 5
//	3 6
//	7 8
//
// leftBars/rightBars give the bits for a bar of height h (0..4) filled from the
// bottom up, for the left and right dot columns respectively.
var leftBars = [5]rune{0x00, 0x40, 0x44, 0x46, 0x47}
var rightBars = [5]rune{0x00, 0x80, 0xA0, 0xB0, 0xB8}

const brailleBase = 0x2800

// Point is a single latency sample's render state.
type Point struct {
	Filled bool // a probe occupies this slot (vs. blank, no data yet)
	Loss   bool // the probe was sent but never answered
	Height int  // bar height 0..4, used when Filled && !Loss
}

// Cell is one rendered braille glyph plus whether it covers a lost probe (the
// caller colors loss cells red).
type Cell struct {
	R    rune
	Loss bool
}

// Glyph builds one braille cell from a left and right sample. A lost probe
// renders as a full-height bar and flags the cell so the caller can highlight it.
func Glyph(left, right Point) Cell {
	lh, lLoss := barHeight(left)
	rh, rLoss := barHeight(right)
	return Cell{
		R:    brailleBase + leftBars[lh] + rightBars[rh],
		Loss: lLoss || rLoss,
	}
}

// Cells pairs consecutive points into braille glyphs.
func Cells(points []Point) []Cell {
	cells := make([]Cell, 0, (len(points)+1)/2)
	for i := 0; i < len(points); i += 2 {
		var right Point
		if i+1 < len(points) {
			right = points[i+1]
		}
		cells = append(cells, Glyph(points[i], right))
	}
	return cells
}

func barHeight(p Point) (height int, loss bool) {
	switch {
	case p.Loss:
		return 4, true
	case !p.Filled:
		return 0, false
	default:
		h := p.Height
		if h < 0 {
			h = 0
		}
		if h > 4 {
			h = 4
		}
		return h, false
	}
}

// String renders cells to a plain string (no color); useful for tests.
func String(cells []Cell) string {
	out := make([]rune, len(cells))
	for i, c := range cells {
		out[i] = c.R
	}
	return string(out)
}
