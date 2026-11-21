package sumfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	h1 = "1111111111111111111111111111111111111111111111111111111111111111"
	h2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

func TestFormatThenParse(t *testing.T) {
	entries := []Entry{
		{URL: "https://github.com/k3s-io/k3s/releases/download/v1.37.2%2Bk3s1/k3s", SHA256: h2},
		{URL: "https://dl.cloudsmith.io/public/caddy/stable/gpg.key", SHA256: h1},
	}
	data, err := Format(entries)
	if err != nil {
		t.Fatal(err)
	}
	want := Header + "\n" +
		"https://dl.cloudsmith.io/public/caddy/stable/gpg.key sha256:" + h1 + "\n" +
		"https://github.com/k3s-io/k3s/releases/download/v1.37.2%2Bk3s1/k3s sha256:" + h2 + "\n"
	if string(data) != want {
		t.Fatalf("Format:\n%s\nwant\n%s", data, want)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Lookup("https://dl.cloudsmith.io/public/caddy/stable/gpg.key"); !ok || got != h1 {
		t.Errorf("Lookup = %q, %v", got, ok)
	}
	if _, ok := s.Lookup("https://example.org/other"); ok {
		t.Error("Lookup found a URL that is not there")
	}
	if es := s.Entries(); len(es) != 2 || es[0].URL > es[1].URL {
		t.Errorf("Entries = %+v", es)
	}
}

func TestParseAcceptsNoHeaderAndAnEmptyFile(t *testing.T) {
	for _, data := range []string{"", Header + "\n", "https://a.example/x sha256:" + h1 + "\n"} {
		if _, err := Parse([]byte(data)); err != nil {
			t.Errorf("Parse(%q): %v", data, err)
		}
	}
	var none *Sums
	if _, ok := none.Lookup("https://a.example/x"); ok || none.Entries() != nil {
		t.Error("a nil Sums has entries")
	}
}

func TestParseRefusesWhatIsNotTheToolsShape(t *testing.T) {
	line := func(u, h string) string { return u + " sha256:" + h + "\n" }
	for name, c := range map[string]struct {
		data, why string
		line      int
	}{
		"duplicate":           {line("https://a.example/x", h1) + line("https://a.example/x", h2), "two lines", 2},
		"duplicate apart":     {line("https://a.example/x", h1) + line("https://b.example/x", h1) + line("https://a.example/x", h1), "two lines", 3},
		"out of order":        {line("https://b.example/x", h1) + line("https://a.example/x", h1), "order", 2},
		"blank line":          {line("https://a.example/x", h1) + "\n" + line("https://b.example/x", h1), "blank", 2},
		"comment":             {line("https://a.example/x", h1) + "# note\n", "comment", 2},
		"other header":        {"# my sums\n" + line("https://a.example/x", h1), "comment", 1},
		"header not first":    {line("https://a.example/x", h1) + Header + "\n", "comment", 2},
		"carriage return":     {"https://a.example/x sha256:" + h1 + "\r\n", "carriage return", 1},
		"no final line feed":  {"https://a.example/x sha256:" + h1, "line feed", 0},
		"http":                {line("http://a.example/x", h1), "https URL", 1},
		"no sha256 prefix":    {"https://a.example/x " + h1 + "\n", "sha256:", 1},
		"uppercase hex":       {line("https://a.example/x", strings.ToUpper("abcdef"+h1[6:])), "sha256:", 1},
		"short hex":           {line("https://a.example/x", h1[:63]), "sha256:", 1},
		"third field":         {"https://a.example/x sha256:" + h1 + " extra\n", "not \"<url> sha256:<hex>\"", 1},
		"two spaces":          {"https://a.example/x  sha256:" + h1 + "\n", "sha256:", 1},
		"tab":                 {"https://a.example/x\tsha256:" + h1 + "\n", "not \"<url> sha256:<hex>\"", 1},
		"control in URL":      {line("https://a.example/\x1bx", h1), "https URL", 1},
		"non-ASCII URL":       {line("https://a.example/é", h1), "https URL", 1},
		"URL only":            {"https://a.example/x\n", "not \"<url> sha256:<hex>\"", 1},
		"too long URL":        {line("https://a.example/"+strings.Repeat("a", MaxURL), h1), "https URL", 1},
		"other algorithm":     {"https://a.example/x sha512:" + h1 + "\n", "sha256:", 1},
		"leading space":       {" https://a.example/x sha256:" + h1 + "\n", "not \"<url> sha256:<hex>\"", 1},
		"empty URL after tag": {line("https://", h1), "https URL", 1},
	} {
		_, err := Parse([]byte(c.data))
		var se *Error
		if !errors.As(err, &se) {
			t.Errorf("%s: Parse = %v, want an *Error", name, err)
			continue
		}
		if !strings.Contains(se.Message, c.why) || se.Line != c.line {
			t.Errorf("%s: line %d %q, want line %d and %q", name, se.Line, se.Message, c.line, c.why)
		}
	}
}

func TestParseRefusesAFileTooLarge(t *testing.T) {
	if _, err := Parse(make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("a file larger than MaxBytes was read")
	}
}

func TestFormatRefusesWhatItCannotWrite(t *testing.T) {
	for name, es := range map[string][]Entry{
		"twice":   {{URL: "https://a.example/x", SHA256: h1}, {URL: "https://a.example/x", SHA256: h2}},
		"http":    {{URL: "http://a.example/x", SHA256: h1}},
		"bad sum": {{URL: "https://a.example/x", SHA256: "abc"}},
		"space":   {{URL: "https://a.example/x y", SHA256: h1}},
	} {
		if _, err := Format(es); err == nil {
			t.Errorf("%s: Format wrote %+v", name, es)
		}
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	s, data, err := Read(dir)
	if err != nil || data != nil || len(s.Entries()) != 0 {
		t.Fatalf("no file: %v, %v, %v", s, data, err)
	}
	os.WriteFile(filepath.Join(dir, Name), nil, 0o644)
	if _, data, err := Read(dir); err != nil || data == nil {
		t.Fatalf("an empty file: data %v, %v (want an empty, not nil, data)", data, err)
	}
	body := "https://a.example/x sha256:" + h1 + "\n"
	os.WriteFile(filepath.Join(dir, Name), []byte(body), 0o644)
	s, data, err = Read(dir)
	if err != nil || string(data) != body {
		t.Fatalf("Read: %q, %v", data, err)
	}
	if h, _ := s.Lookup("https://a.example/x"); h != h1 {
		t.Errorf("Lookup = %q", h)
	}
	os.WriteFile(filepath.Join(dir, Name), []byte("garbage\n"), 0o644)
	var se *Error
	if _, _, err := Read(dir); !errors.As(err, &se) {
		t.Errorf("a malformed file: %v", err)
	}
}

// mamela.sum is the declaration's own file: a link (to a file anywhere) is refused, as the
// Control Plane's tree refuses links.
func TestReadRefusesALinkAndADirectory(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(other, []byte("https://a.example/x sha256:"+h1+"\n"), 0o644)
	if err := os.Symlink(other, filepath.Join(dir, Name)); err != nil {
		t.Skip(err)
	}
	var se *Error
	if _, _, err := Read(dir); !errors.As(err, &se) || !strings.Contains(se.Message, "not a regular file") {
		t.Errorf("a link: %v", err)
	}
	dir2 := t.TempDir()
	os.Mkdir(filepath.Join(dir2, Name), 0o755)
	if _, _, err := Read(dir2); !errors.As(err, &se) {
		t.Errorf("a directory: %v", err)
	}
}
