// Package sumfile reads and writes mamela.sum, the sha256 of what a declaration downloads, by URL
// (docs/design/declaration-inputs.md, 2). `mamela pin` writes it; evaluating a declaration reads it
// from the root of the declaration's Pome, and never writes it or downloads anything.
//
// The file is one line per URL, in the byte order of the URLs, each ending with a line feed:
//
//	# Written by `mamela pin`: the sha256 of what this declaration downloads. Do not edit.
//	https://example.org/app-1.0.tar.gz sha256:<64 lowercase hex digits>
//
// The first line may be the header above, exactly; there are no other comments and no blank
// lines. Anything else (two lines of one URL, lines out of order, another shape) is refused: the
// file is the tool's, and a person reviews its changes, not its layout.
package sumfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/names"
)

// Name is the file's name, at the root of the declaration's Pome (next to Pome.toml).
const Name = "mamela.sum"

// Header is the first line mamela pin writes (without its line feed).
const Header = "# Written by `mamela pin`: the sha256 of what this declaration downloads. Do not edit."

// MaxBytes bounds the file.
const MaxBytes = 4 << 20

// MaxURL bounds a URL of the file, in bytes (an artifact's URL is at most as long).
const MaxURL = 2048

// Entry is one line: the sha256 (hex, without "sha256:") of the bytes at URL.
type Entry struct {
	URL    string
	SHA256 string
}

// Sums is a parsed mamela.sum. The zero value (and nil) has no entry.
type Sums struct {
	entries []Entry // in the order of their URLs
	byURL   map[string]string
}

// Lookup is the sha256 the file has for url.
func (s *Sums) Lookup(url string) (string, bool) {
	if s == nil {
		return "", false
	}
	h, ok := s.byURL[url]
	return h, ok
}

// Entries are the file's lines, in the order of their URLs.
func (s *Sums) Entries() []Entry {
	if s == nil {
		return nil
	}
	return slices.Clone(s.entries)
}

// Error is why a mamela.sum is refused: the line (0 for the whole file) and what is wrong with it.
type Error struct {
	Line    int
	Message string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s, line %d: %s", Name, e.Line, e.Message)
	}
	return Name + ": " + e.Message
}

// ValidURL says whether u can be a line's URL: https, at most MaxURL bytes of printable ASCII
// without a space. The kinds' own checks are narrower; this one keeps a line one URL and one sum.
func ValidURL(u string) bool {
	if !strings.HasPrefix(u, "https://") || len(u) <= len("https://") || len(u) > MaxURL {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] <= ' ' || u[i] > '~' {
			return false
		}
	}
	return true
}

// Parse reads a mamela.sum strictly.
func Parse(data []byte) (*Sums, error) {
	if len(data) > MaxBytes {
		return nil, &Error{Message: fmt.Sprintf("the file is %d bytes, more than %d", len(data), MaxBytes)}
	}
	s := &Sums{byURL: map[string]string{}}
	if len(data) == 0 {
		return s, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, &Error{Message: "the last line does not end with a line feed"}
	}
	lines := strings.Split(string(data[:len(data)-1]), "\n")
	for i, line := range lines {
		n := i + 1
		switch {
		case i == 0 && line == Header:
			continue
		case strings.HasSuffix(line, "\r"):
			return nil, &Error{Line: n, Message: "the line ends with a carriage return (the file's lines end with a line feed only)"}
		case line == "":
			return nil, &Error{Line: n, Message: "the line is blank"}
		case strings.HasPrefix(line, "#"):
			return nil, &Error{Line: n, Message: "a comment other than the header on the first line"}
		}
		url, sum, ok := strings.Cut(line, " ")
		hex, isSum := strings.CutPrefix(sum, "sha256:")
		switch {
		case !ok || strings.Contains(sum, " "):
			return nil, &Error{Line: n, Message: fmt.Sprintf("%s is not \"<url> sha256:<hex>\"", strconv.Quote(line))}
		case !ValidURL(url):
			return nil, &Error{Line: n, Message: fmt.Sprintf("%s is not an https URL of printable characters without a space, of at most %d bytes", strconv.Quote(url), MaxURL)}
		case !isSum || !names.SHA256(hex):
			return nil, &Error{Line: n, Message: fmt.Sprintf("%s is not sha256: and 64 lowercase hex digits", strconv.Quote(sum))}
		}
		if _, twice := s.byURL[url]; twice {
			return nil, &Error{Line: n, Message: fmt.Sprintf("%s is on two lines", url)}
		}
		if last := len(s.entries) - 1; last >= 0 && s.entries[last].URL > url {
			return nil, &Error{Line: n, Message: fmt.Sprintf("%s comes after %s: the lines are in the order of their URLs", url, s.entries[last].URL)}
		}
		s.entries = append(s.entries, Entry{URL: url, SHA256: hex})
		s.byURL[url] = hex
	}
	return s, nil
}

// Format is the file of entries: the header, then one line per entry in the order of the URLs.
// Each URL must be valid (ValidURL) and once only, and each sha256 64 lowercase hex digits.
func Format(entries []Entry) ([]byte, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.URL, b.URL) })
	var b bytes.Buffer
	b.WriteString(Header + "\n")
	for i, e := range sorted {
		switch {
		case !ValidURL(e.URL):
			return nil, fmt.Errorf("%s cannot be a line of %s", strconv.Quote(e.URL), Name)
		case !names.SHA256(e.SHA256):
			return nil, fmt.Errorf("%s: %s is not a sha256", e.URL, strconv.Quote(e.SHA256))
		case i > 0 && sorted[i-1].URL == e.URL:
			return nil, fmt.Errorf("%s is twice in the entries", e.URL)
		}
		fmt.Fprintf(&b, "%s sha256:%s\n", e.URL, e.SHA256)
	}
	return b.Bytes(), nil
}

// Read reads dir's mamela.sum. When there is none, data is nil and the sums are empty; when there
// is one, data is its bytes (not nil, even when it is empty). It must be a regular file (not a
// link) of at most MaxBytes.
func Read(dir string) (s *Sums, data []byte, err error) {
	p := filepath.Join(dir, Name)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Sums{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, &Error{Message: "it is not a regular file (a link, a directory): it must be the file mamela pin writes, in the declaration's own tree"}
	}
	f, err := os.OpenFile(p, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil {
		return nil, nil, err
	} else if !os.SameFile(fi, opened) {
		return nil, nil, &Error{Message: "it changed while it was read; read it again"}
	}
	data, err = io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if data == nil {
		data = []byte{}
	}
	s, err = Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return s, data, nil
}
