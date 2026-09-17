package app

import (
	"strconv"
	"unicode/utf8"
)

// RenderExternal renders one externally sourced string as a single display
// field: raw when it is valid UTF-8 with no control character (C0, DEL,
// C1), no double quote and no backslash — the shapes that could otherwise
// forge a line boundary (a newline inserting a fake protocol line), emit a
// terminal control sequence, or make the two rendering forms ambiguous.
// Otherwise it renders as Go's quoted-string form (strconv.Quote), which
// escapes exactly those bytes and always starts with a double quote — so a
// raw rendering never starts with one, and a reader can always tell which
// form a field took. Ordinary paths and identifiers render untouched. It
// is the one rendering boundary cmd/hop's status and retirement output use
// for such fields, and the one the application uses for a durable value it
// embeds in a detail sentence, such as a launch's creation label.
func RenderExternal(s string) string {
	if isSafeExternalString(s) {
		return s
	}
	return strconv.Quote(s)
}

// isSafeExternalString reports whether s can render raw per
// RenderExternal's contract.
func isSafeExternalString(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			return false
		case r < 0x20 || r == 0x7f: // C0 controls and DEL
			return false
		case r >= 0x80 && r <= 0x9f: // C1 controls
			return false
		}
	}
	return true
}
