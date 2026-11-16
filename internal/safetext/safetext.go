// Package safetext keeps what a person reads the same as what runs.
//
// A plan is read to be approved, and an error is read to be fixed, in a terminal or in a pull
// request. Text from a declaration (a k3s manifest, which k3s applies with cluster-admin rights,
// or a provider's values) can hold characters that do not show as themselves: a carriage return
// overwrites the line, an escape rewrites the screen, a bidirectional control reorders what is
// shown, a zero-width character is not shown at all. Such text can make a plan look like it
// changes something other than what it changes.
//
// The graph refuses the text (Find), and everything that quotes text to a person shows it
// escaped (Escape): two defences, so that neither has to be perfect.
package safetext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hidden reports whether r does not show as itself: a control character (C0, DEL, C1), a line or
// paragraph separator, or a format character (bidirectional controls, zero-width characters,
// the byte order mark, tag characters and the like).
func Hidden(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' '
}

// Finding is a character of a text that does not show as itself, and where it is.
type Finding struct {
	Rune         rune
	Line, Column int // 1-based; the column counts characters
}

// String says what and where, for a person: "U+202E (right-to-left override) at line 3, column 5".
func (f Finding) String() string {
	return fmt.Sprintf("%s at line %d, column %d", Describe(f.Rune), f.Line, f.Column)
}

// Find returns the first character of s that does not show as itself. multiline allows the
// newline and the tab, which text of several lines (a manifest) needs; every other control
// character is refused, the carriage return included: a manifest is written with LF line endings.
// Bytes that are not UTF-8 are found too (their Rune is utf8.RuneError).
func Find(s string, multiline bool) (Finding, bool) {
	line, col := 1, 1
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n == 1:
			return Finding{Rune: r, Line: line, Column: col}, true
		case r == '\n' && multiline:
			line, col = line+1, 1
			continue
		case r == '\t' && multiline:
		case Hidden(r):
			return Finding{Rune: r, Line: line, Column: col}, true
		}
		col++
	}
	return Finding{}, false
}

var names = map[rune]string{
	0x00: "NUL", 0x07: "bell", 0x08: "backspace", 0x09: "tab", 0x0a: "newline", 0x0b: "vertical tab", 0x0c: "form feed",
	0x0d: "carriage return", 0x1b: "escape", 0x7f: "delete", 0x85: "next line",
	0xad: "soft hyphen", 0x61c: "arabic letter mark",
	0x180e: "mongolian vowel separator", 0x200b: "zero-width space", 0x200c: "zero-width non-joiner", 0x200d: "zero-width joiner",
	0x200e: "left-to-right mark", 0x200f: "right-to-left mark", 0x2028: "line separator", 0x2029: "paragraph separator",
	0x202a: "left-to-right embedding", 0x202b: "right-to-left embedding", 0x202c: "pop directional formatting",
	0x202d: "left-to-right override", 0x202e: "right-to-left override", 0x2060: "word joiner",
	0x2066: "left-to-right isolate", 0x2067: "right-to-left isolate", 0x2068: "first strong isolate", 0x2069: "pop directional isolate",
	0xfeff: "byte order mark", utf8.RuneError: "invalid UTF-8",
}

// Describe names a character for a person: "U+202E (right-to-left override)".
func Describe(r rune) string {
	if r == utf8.RuneError {
		return "an invalid UTF-8 byte"
	}
	name, ok := names[r]
	switch {
	case ok:
	case r < 0x20:
		name = "control character"
	case r >= 0x80 && r < 0xa0:
		name = "control character"
	case r >= 0xe0000 && r <= 0xe007f:
		name = "tag character"
	default:
		name = "invisible character"
	}
	return fmt.Sprintf("U+%04X (%s)", r, name)
}

// Escape is s with every character that does not show as itself written out: \n, \r, \t, \x1b,
// ‮, \U000e0041, and \xff for a byte that is not UTF-8. Ordinary text, including non-ASCII
// letters, is returned as it is.
func Escape(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case Hidden(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// EscapeLines is Escape for text of several lines: the lines stay lines (split on '\n'), and each
// one is escaped. A tab is written as \t, so that it cannot move what follows it.
func EscapeLines(s string) []string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = Escape(l)
	}
	return lines
}
