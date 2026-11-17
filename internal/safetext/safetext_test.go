package safetext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFind(t *testing.T) {
	for _, c := range []struct {
		name      string
		in        string
		multiline bool
		want      string // "" when the text is clean
	}{
		{"ordinary", "kind: Namespace", false, ""},
		{"non-ASCII text", "name: é日本語 \U0001F600", false, ""},
		{"a newline and a tab in text of lines", "a:\n\tb\n", true, ""},
		{"a newline in a single line", "a\nb", false, "U+000A (newline) at line 1, column 2"},
		{"a tab in a single line", "a\tb", false, "U+0009 (tab) at line 1, column 2"},
		{"a carriage return in text of lines", "a\r\nb", true, "U+000D (carriage return) at line 1, column 2"},
		{"an escape on the third line", "a\nb\n  \x1b[2K", true, "U+001B (escape) at line 3, column 3"},
		{"NUL", "a\x00", true, "U+0000 (NUL) at line 1, column 2"},
		{"delete", "\x7f", true, "U+007F (delete) at line 1, column 1"},
		{"a C1 control", "a\u009bb", true, "U+009B (control character) at line 1, column 2"},
		{"a next line", "a\u0085", true, "U+0085 (next line) at line 1, column 2"},
		{"a right-to-left override", "a\u202eb", true, "U+202E (right-to-left override) at line 1, column 2"},
		{"a left-to-right isolate", "\u2066", true, "U+2066 (left-to-right isolate) at line 1, column 1"},
		{"a zero-width space", "a\u200b", true, "U+200B (zero-width space) at line 1, column 2"},
		{"a zero-width joiner", "a\u200d", true, "U+200D (zero-width joiner) at line 1, column 2"},
		{"a byte order mark", "\ufeff", true, "U+FEFF (byte order mark) at line 1, column 1"},
		{"a line separator", "a\u2028", true, "U+2028 (line separator) at line 1, column 2"},
		{"a tag character", "a\U000e0041", true, "U+E0041 (tag character) at line 1, column 2"},
		{"a byte that is not UTF-8", "ab\xff", true, "an invalid UTF-8 byte at line 1, column 3"},
		{"a replacement character that is UTF-8", "a\ufffd", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, bad := Find(c.in, c.multiline)
			switch {
			case c.want == "" && bad:
				t.Errorf("found %s in %q", f, c.in)
			case c.want != "" && !bad:
				t.Errorf("found nothing in %q; want %s", c.in, c.want)
			case bad && f.String() != c.want:
				t.Errorf("found %q; want %q", f.String(), c.want)
			}
		})
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"plain text, é日本語":      "plain text, é日本語",
		"a\nb":                  `a\nb`,
		"a\r\n":                 `a\r\n`,
		"a\tb":                  `a\tb`,
		"\x1b[2K":               `\x1b[2K`,
		"a\x00b":                `a\x00b`,
		"x\u202ey":              `x\u202ey`,
		"x\u200by":              `x\u200by`,
		"x\u0085y":              `x\x85y`,
		"x\U000e0041y":          `x\U000e0041y`,
		"x\xffy":                `x\xffy`,
		"a\ufffdb":              "a\ufffdb",
		"quotes \" and \\ stay": `quotes " and \ stay`,
	} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever the text, what Escape returns has nothing that does not show as itself, and is valid
// UTF-8: it can be written to a terminal or a comment as it is.
func FuzzEscape(f *testing.F) {
	for _, s := range []string{"", "a", "a\r\nb", "\x1b[2K", "\u202e", "\xff\xfe", "é\u200b", "\U000e0001"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Escape(s)
		if !utf8.ValidString(out) {
			t.Fatalf("Escape(%q) = %q is not UTF-8", s, out)
		}
		if _, bad := Find(out, false); bad {
			t.Fatalf("Escape(%q) = %q still has a character that does not show as itself", s, out)
		}
	})
}

func TestEscapeLines(t *testing.T) {
	got := EscapeLines("a: b\r\n= forged\n\x1b[2K\n")
	want := []string{`a: b\r`, "= forged", `\x1b[2K`, ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("EscapeLines = %q, want %q", got, want)
	}
}
