package sparkline

import "testing"

func TestCellsHeights(t *testing.T) {
	// Two full-height bars -> the fully-filled braille cell U+28FF.
	cells := Cells([]Point{
		{Filled: true, Height: 4},
		{Filled: true, Height: 4},
	})
	if len(cells) != 1 {
		t.Fatalf("want 1 cell, got %d", len(cells))
	}
	if cells[0].R != '⣿' {
		t.Errorf("height 4+4: want ⣿ (U+28FF), got %q (U+%04X)", cells[0].R, cells[0].R)
	}
	if cells[0].Loss {
		t.Errorf("height 4+4 should not be a loss cell")
	}
}

func TestCellsEmptyIsBlank(t *testing.T) {
	cells := Cells([]Point{{}, {}})
	if cells[0].R != '⠀' { // U+2800 blank braille
		t.Errorf("empty: want blank braille U+2800, got U+%04X", cells[0].R)
	}
}

func TestLossIsFullHeightAndFlagged(t *testing.T) {
	cells := Cells([]Point{{Filled: true, Loss: true}, {}})
	if !cells[0].Loss {
		t.Errorf("loss point should flag the cell")
	}
	// Left column full (0x47), right empty -> U+2800 + 0x47.
	if cells[0].R != rune(brailleBase+0x47) {
		t.Errorf("loss left bar: got U+%04X", cells[0].R)
	}
}

func TestOddPointCountPairsTrailingBlank(t *testing.T) {
	cells := Cells([]Point{{Filled: true, Height: 1}})
	if len(cells) != 1 {
		t.Fatalf("want 1 cell, got %d", len(cells))
	}
	// height 1 left (bottom dot 0x40), empty right.
	if cells[0].R != rune(brailleBase+0x40) {
		t.Errorf("got U+%04X", cells[0].R)
	}
}

func TestMonotonicHeights(t *testing.T) {
	// Each height should be a superset of dots of the one below it (filled from
	// the bottom), so rendering climbs visually.
	for h := 1; h <= 4; h++ {
		prev := leftBars[h-1]
		cur := leftBars[h]
		if cur&prev != prev {
			t.Errorf("left height %d (%08b) is not a superset of %d (%08b)", h, cur, h-1, prev)
		}
	}
}
