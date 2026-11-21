package evaluate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/safetext"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

// A declaration may take a text from a file of its own Pome (contentFrom, graph.Files): a unit, a
// manifest, kept as a file of its own with its extension. Evaluating reads it from the Pome's
// directory, the same tree the declaration was built from (in the Control Plane's API, the commit's
// tree), and nothing else: a relative path, inside the Pome, through no symbolic link, to a regular
// file of UTF-8 text, within the kind's limit and, in all, within MaxFileTexts. The bytes are taken
// as they are (no newline is changed, nothing is trimmed), so the graph is the one the same text
// written in would make.

// maxFilePath bounds the path a spec names.
const maxFilePath = 1024

// pomeFiles reads the files of the Pome at root for one evaluation.
type pomeFiles struct {
	root *os.Root
	left int // what the declaration may still take from its files, in bytes
	max  int
	sums *sumfile.Sums // its mamela.sum (sums.go); nil: none
}

func newPomeFiles(dir string, max int) (*pomeFiles, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &pomeFiles{root: root, left: max, max: max}, nil
}

func (f *pomeFiles) Close() error { return f.root.Close() }

func fileError(code Code, message, hint string) error {
	return &graph.FileError{Code: graph.Code(code), Message: message, Hint: hint}
}

const (
	hintRelative = "write the path relative to the Pome's directory (where Pome.toml is), as path.Clean writes it, " +
		"such as \"files/app.service\": a declaration takes only its own Pome's files"
	hintMissing = "check the path: it is relative to the Pome's directory (where Pome.toml is). Commit the file: " +
		"the Control Plane evaluates the commit's tree and nothing else"
	hintLink = "put the file itself in the Pome, not a link to it: Mamela does not follow links " +
		"(and the Control Plane refuses a commit's tree that has one)"
	hintText = "a text is UTF-8 without NUL: save the file as UTF-8. A binary file is no text of a declaration " +
		"(a release's binary is a download pinned by its sha256, NodeArtifact)"
)

// Text is the text of the file at p (graph.Files).
func (f *pomeFiles) Text(p string, max int) (string, error) {
	if err := checkFilePath(p); err != nil {
		return "", err
	}
	segs := strings.Split(p, "/")
	var seen fs.FileInfo
	for i := range segs {
		at := strings.Join(segs[:i+1], "/")
		fi, err := f.root.Lstat(at)
		last := i == len(segs)-1
		switch {
		case errors.Is(err, fs.ErrNotExist):
			why := "there is no such file"
			if !last {
				why = "there is no directory " + at
			}
			return "", fileError(CodeFileMissing, fmt.Sprintf("%s is not in the Pome: %s", p, why), hintMissing)
		case err != nil:
			return "", fileError(CodeFileMissing, fmt.Sprintf("%s cannot be read: %s", p, reason(err)), hintMissing)
		case fi.Mode()&fs.ModeSymlink != 0:
			what := "the file " + p
			if !last {
				what = "the directory " + at + " on the way to " + p
			}
			return "", fileError(CodeFileLink, what+" is a symbolic link", hintLink)
		case !last && !fi.IsDir():
			return "", fileError(CodeFileMissing, fmt.Sprintf("%s is not in the Pome: %s is not a directory", p, at), hintMissing)
		case last && !fi.Mode().IsRegular():
			return "", fileError(CodeFileNotRegular, fmt.Sprintf("%s is %s, not a regular file", p, fileType(fi.Mode())),
				"name a regular file of the Pome")
		case last && fi.Size() > int64(max):
			return "", tooLarge(p, fi.Size(), max)
		}
		seen = fi
	}

	// Not waiting on what is not a file (O_NONBLOCK) should it have been put in its place since: what
	// is opened must be the regular file looked at.
	file, err := f.root.OpenFile(p, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return "", fileError(CodeFileMissing, fmt.Sprintf("%s cannot be read: %s", p, reason(err)), hintMissing)
	}
	defer file.Close()
	if fi, err := file.Stat(); err != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, seen) {
		return "", fileError(CodeFileMissing, fmt.Sprintf("%s cannot be read: it changed while Mamela read it", p),
			"evaluate again once the Pome is not being changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	if err != nil {
		return "", fileError(CodeFileMissing, fmt.Sprintf("%s cannot be read: %s", p, reason(err)), hintMissing)
	}
	if len(data) > max {
		return "", tooLarge(p, int64(len(data)), max)
	}
	if !utf8.Valid(data) {
		at := bytesValidUTF8Prefix(data)
		return "", fileError(CodeFileNotText, fmt.Sprintf("%s is not text: byte %d (line %d) is not UTF-8", p, at+1, lineAt(data, at)), hintText)
	}
	if at := bytes.IndexByte(data, 0); at >= 0 {
		return "", fileError(CodeFileNotText, fmt.Sprintf("%s is not text: byte %d (line %d) is NUL", p, at+1, lineAt(data, at)), hintText)
	}
	if f.left -= len(data); f.left < 0 {
		return "", fileError(CodeFileTooLarge,
			fmt.Sprintf("%s is one file too many: the texts the declaration takes from its files come to more than %d bytes in all", p, f.max),
			"take fewer or smaller files: a large file is a download pinned by its sha256 (NodeArtifact), not a text of the declaration")
	}
	return string(data), nil
}

// checkFilePath refuses a path that is not a clean relative path inside the Pome.
func checkFilePath(p string) error {
	bad := func(why string) error {
		return fileError(CodeFileOutside, fmt.Sprintf("%q is not a path of a file of the Pome: %s", p, why), hintRelative)
	}
	switch {
	case p == "":
		return bad("it is empty")
	case len(p) > maxFilePath:
		return bad(fmt.Sprintf("it is longer than %d bytes", maxFilePath))
	case !utf8.ValidString(p):
		return bad("it is not UTF-8")
	case strings.Contains(p, `\`):
		return bad(`it has a backslash (the separator is "/")`)
	case path.IsAbs(p):
		return bad("it is absolute")
	case slices.Contains(strings.Split(p, "/"), ".."):
		return bad("it goes up (..), out of the Pome")
	case p == ".":
		return bad("it is the Pome's directory itself")
	case path.Clean(p) != p:
		return bad(fmt.Sprintf("it is not clean (write %q)", path.Clean(p)))
	}
	if fd, bad2 := safetext.Find(p, false); bad2 {
		return bad("it has " + fd.String())
	}
	return nil
}

func tooLarge(p string, size int64, max int) error {
	return fileError(CodeFileTooLarge, fmt.Sprintf("%s is %d bytes; the text it is taken for is at most %d bytes", p, size, max),
		"keep the file under the limit: a larger file is a download pinned by its sha256 (NodeArtifact), and a larger Kubernetes "+
			"application a HelmChart that installs a chart")
}

// reason is why an operation on a file failed, without the path (the error's path is the Pome's
// directory on this machine, which the Control Plane's answers do not show).
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func fileType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeDevice != 0:
		return "a device"
	}
	return "a special file"
}

// bytesValidUTF8Prefix is the offset of the first byte of data that is not UTF-8.
func bytesValidUTF8Prefix(data []byte) int {
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && n == 1 {
			return i
		}
		i += n
	}
	return len(data)
}

// lineAt is the line (from 1) of the byte at offset i.
func lineAt(data []byte, i int) int { return bytes.Count(data[:i], []byte("\n")) + 1 }
