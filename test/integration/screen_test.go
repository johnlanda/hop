package integration

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// screenBuffer is a minimal terminal screen model: a fixed grid that a
// cursor-addressed TUI paints into. It applies cursor positioning, erase and
// the common cursor moves, and ignores styling, so the rendered text reflects
// the CURRENT screen rather than the accumulated scrollback history. That is
// what makes an ordering assertion trustworthy: stale earlier frames are
// overwritten in place, not left to satisfy the check.
type screenBuffer struct {
	rows, cols int
	grid       [][]rune
	row, col   int
}

// newScreenBuffer returns a blank screen of the given size.
func newScreenBuffer(rows, cols int) *screenBuffer {
	grid := make([][]rune, rows)
	for r := range grid {
		grid[r] = blankRow(cols)
	}
	return &screenBuffer{rows: rows, cols: cols, grid: grid}
}

func blankRow(cols int) []rune {
	row := make([]rune, cols)
	for c := range row {
		row[c] = ' '
	}
	return row
}

// Write applies a chunk of terminal output to the screen. It may be called
// repeatedly; the grid accumulates in place.
func (s *screenBuffer) Write(p []byte) {
	for i := 0; i < len(p); {
		b := p[i]
		switch {
		case b == 0x1b:
			i += s.applyEscape(p[i:])
		case b == '\r':
			s.col = 0
			i++
		case b == '\n':
			s.newline()
			i++
		case b == '\t':
			s.col = clampIndex((s.col/8+1)*8, s.cols-1)
			i++
		case b == '\b':
			s.col = clampIndex(s.col-1, s.cols-1)
			i++
		case b < 0x20 || b == 0x7f:
			i++ // other control bytes have no effect on the model
		default:
			r, size := utf8.DecodeRune(p[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			s.put(r)
			i += size
		}
	}
}

// applyEscape handles one escape sequence starting at p[0] == ESC and returns
// how many bytes it consumed.
func (s *screenBuffer) applyEscape(p []byte) int {
	if len(p) < 2 {
		return len(p)
	}
	switch p[1] {
	case '[':
		return s.applyCSI(p)
	case ']':
		return consumeOSC(p)
	case '(', ')', '*', '+':
		return 3 // charset designation: ESC, kind, one byte
	default:
		return 2 // ignore other two-byte escapes (keypad modes, etc.)
	}
}

// applyCSI handles a CSI sequence (ESC [ params intermediates final) and
// returns the bytes consumed.
func (s *screenBuffer) applyCSI(p []byte) int {
	i := 2
	start := i
	for i < len(p) && (p[i] >= '0' && p[i] <= '9' || p[i] == ';' || p[i] == '?') {
		i++
	}
	params := string(p[start:i])
	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f { // intermediates
		i++
	}
	if i >= len(p) {
		return len(p)
	}
	final := p[i]
	i++
	s.dispatchCSI(final, params)
	return i
}

// dispatchCSI applies the handful of CSI commands that move the cursor or
// erase; every other command (styling, modes) is ignored.
func (s *screenBuffer) dispatchCSI(final byte, params string) {
	switch final {
	case 'H', 'f': // cursor position, 1-based row;col
		row, col := 1, 1
		fields := strings.Split(params, ";")
		if len(fields) > 0 {
			row = atoiDefault(fields[0], 1)
		}
		if len(fields) > 1 {
			col = atoiDefault(fields[1], 1)
		}
		s.row = clampIndex(row-1, s.rows-1)
		s.col = clampIndex(col-1, s.cols-1)
	case 'A':
		s.row = clampIndex(s.row-atoiDefault(params, 1), s.rows-1)
	case 'B':
		s.row = clampIndex(s.row+atoiDefault(params, 1), s.rows-1)
	case 'C':
		s.col = clampIndex(s.col+atoiDefault(params, 1), s.cols-1)
	case 'D':
		s.col = clampIndex(s.col-atoiDefault(params, 1), s.cols-1)
	case 'J':
		s.eraseDisplay(atoiDefault(params, 0))
	case 'K':
		s.eraseLine(atoiDefault(params, 0))
	}
}

// eraseDisplay clears part or all of the screen.
func (s *screenBuffer) eraseDisplay(mode int) {
	switch mode {
	case 2, 3:
		for r := range s.grid {
			s.grid[r] = blankRow(s.cols)
		}
	case 1:
		for r := 0; r < s.row; r++ {
			s.grid[r] = blankRow(s.cols)
		}
		s.clearLineRange(s.row, 0, s.col)
	default: // 0: cursor to end
		s.clearLineRange(s.row, s.col, s.cols-1)
		for r := s.row + 1; r < s.rows; r++ {
			s.grid[r] = blankRow(s.cols)
		}
	}
}

// eraseLine clears part or all of the cursor's line.
func (s *screenBuffer) eraseLine(mode int) {
	switch mode {
	case 2:
		s.grid[s.row] = blankRow(s.cols)
	case 1:
		s.clearLineRange(s.row, 0, s.col)
	default:
		s.clearLineRange(s.row, s.col, s.cols-1)
	}
}

func (s *screenBuffer) clearLineRange(row, from, to int) {
	for c := from; c <= to && c < s.cols; c++ {
		s.grid[row][c] = ' '
	}
}

// put writes one rune at the cursor and advances, wrapping to the next row at
// the right edge as a terminal does.
func (s *screenBuffer) put(r rune) {
	if s.col >= s.cols {
		s.col = 0
		s.newline()
	}
	s.grid[s.row][s.col] = r
	s.col++
}

// newline moves to the next row, staying on the last row rather than
// scrolling, which is adequate for a full-screen TUI that repaints by
// absolute addressing.
func (s *screenBuffer) newline() {
	if s.row < s.rows-1 {
		s.row++
	}
}

// Text returns the visible screen as rows joined by newlines, with trailing
// blank cells and trailing blank rows trimmed.
func (s *screenBuffer) Text() string {
	lines := make([]string, s.rows)
	for r := range s.grid {
		lines[r] = strings.TrimRight(string(s.grid[r]), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// consumeOSC returns the length of an OSC sequence (ESC ] … terminator),
// where the terminator is BEL (0x07) or ST (ESC \). The content is ignored.
func consumeOSC(p []byte) int {
	i := 2
	for i < len(p) {
		if p[i] == 0x07 {
			return i + 1
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return len(p)
}

// clampIndex clamps v into the inclusive range [0, hi]; grid indices never go
// below zero.
func clampIndex(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestScreenBufferRendersCurrentScreenNotHistory(t *testing.T) {
	screen := newScreenBuffer(5, 20)
	// An early frame paints a stale line, then the frame is cleared and a new
	// line painted at the same place. Only the new content must survive.
	screen.Write([]byte("\x1b[1;1Hstale-manager"))
	screen.Write([]byte("\x1b[2J\x1b[1;1Hfresh-line"))

	text := screen.Text()

	if strings.Contains(text, "stale-manager") {
		t.Errorf("screen retained cleared history:\n%s", text)
	}
	if !strings.Contains(text, "fresh-line") {
		t.Errorf("screen lost the current frame:\n%s", text)
	}
}

func TestScreenBufferStripsStyling(t *testing.T) {
	screen := newScreenBuffer(3, 30)
	// SGR color codes and an OSC title must leave no payload in the text.
	screen.Write([]byte("\x1b]0;a title\x07\x1b[1;1H\x1b[31mmanager\x1b[0m"))

	text := screen.Text()

	if text != "manager" {
		t.Errorf("Text() = %q, want just \"manager\" with all escapes removed", text)
	}
}

func TestScreenBufferPlacesRowsByPosition(t *testing.T) {
	screen := newScreenBuffer(5, 20)
	// Paint the rows out of order by absolute position; the model must place
	// each on its addressed row so the rendered order is by row, not by the
	// order the bytes arrived.
	screen.Write([]byte("\x1b[3;1Hreviewer"))
	screen.Write([]byte("\x1b[1;1Hmanager"))
	screen.Write([]byte("\x1b[2;1Himplementer"))

	text := screen.Text()

	managerAt := strings.Index(text, "manager")
	implementerAt := strings.Index(text, "implementer")
	reviewerAt := strings.Index(text, "reviewer")
	if managerAt >= implementerAt || implementerAt >= reviewerAt {
		t.Errorf("rendered order manager=%d implementer=%d reviewer=%d, want manager first\n%s",
			managerAt, implementerAt, reviewerAt, text)
	}
}

func TestScreenBufferReversedRowsFailOrdering(t *testing.T) {
	screen := newScreenBuffer(5, 20)
	// The manager drawn BELOW the workers must not satisfy a manager-first
	// check: this guards the PTY assertion against a reversed screen.
	screen.Write([]byte("\x1b[1;1Himplementer"))
	screen.Write([]byte("\x1b[2;1Hreviewer"))
	screen.Write([]byte("\x1b[3;1Hmanager"))

	text := screen.Text()

	managerAt := strings.Index(text, "manager")
	implementerAt := strings.Index(text, "implementer")
	reviewerAt := strings.Index(text, "reviewer")
	if managerAt < implementerAt && managerAt < reviewerAt {
		t.Errorf("reversed screen wrongly passed manager-first: manager=%d implementer=%d reviewer=%d\n%s",
			managerAt, implementerAt, reviewerAt, text)
	}
}
