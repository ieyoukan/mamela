package graph

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

// A spec may take a long text (a unit, a manifest) from a file of the declaration's Pome instead of
// writing it (docs/design/declaration-inputs.md): "contentFrom": "files/app.service" for
// "content". The core knows no member of any kind: a kind's Decode asks for the text with
// SpecDecoder.Text, and the evaluator, which knows the Pome, reads it (Files). The text replaces the
// name before the graph is validated, so that everything after (validation, the canonical form, the
// digest, the plan, the revision) sees what it would have seen had the text been written in.

// Files are the files of the Pome being evaluated, which a spec may take a text from. Only the
// evaluation of a Pome has them: a graph read back from the database has its texts written in.
type Files interface {
	// Text is the text of the file at path, relative to the root of the Pome (the directory of its
	// Pome.toml), exactly as it is: at most max bytes of UTF-8 without NUL. What cannot be taken is a
	// *FileError, which says why and what to do.
	Text(path string, max int) (string, error)
}

// FileError is why a file that a spec names cannot be taken: a stable code (the evaluator's,
// MAM-EVAL-05xx), what is wrong, and what to do.
type FileError struct {
	Code    Code
	Message string
	Hint    string
}

func (e *FileError) Error() string { return string(e.Code) + ": " + e.Message }

// Text is the text member field ("content") of the spec: written in it (inline), or read from the
// file of the Pome that the member field+"From" ("contentFrom", given raw as from) names; exactly
// one of the two. The file's text is at most max bytes. nil, with the problem recorded, when there
// is no text.
func (d *SpecDecoder) Text(field string, inline *string, from jsontext.Value, max int) *string {
	fromField := field + "From"
	if from != nil && string(from) != "null" && inline != nil {
		d.Invalid(CodeInvalidValue, fromField, fmt.Sprintf("%s has both spec.%s and spec.%s", d.kind, field, fromField),
			fmt.Sprintf("keep one: the text itself in %s, or the path of the file of the Pome that has it in %s", field, fromField))
		return nil
	}
	path := d.OptionalString(from, fromField)
	switch {
	case inline != nil:
		return inline
	case from != nil && path == nil:
		return nil // null: OptionalString said so
	case path == nil:
		d.Need(false, field) // as it always was said (a spec of before contentFrom)
		return nil
	case d.files == nil:
		d.problems = append(d.problems, Problem{Code: CodeUnknownField, Path: d.where + " spec." + fromField,
			Message: fmt.Sprintf("spec.%s names a file of a Pome (%q), and this graph is not the output of a Pome being evaluated", fromField, *path),
			Hint:    fmt.Sprintf("evaluate the Pome (mamela eval, check or plan <pome-dir>), which reads the file; or write the text in %s", field)})
		return nil
	}
	text, err := d.files.Text(*path, max)
	if err != nil {
		var fe *FileError
		if !errors.As(err, &fe) {
			fe = &FileError{Code: CodeInvalidValue, Message: err.Error()}
		}
		d.problems = append(d.problems, Problem{Code: fe.Code, Path: d.where + " spec." + fromField, Message: fe.Message, Hint: fe.Hint})
		return nil
	}
	if d.sources == nil {
		d.sources = map[string]string{}
	}
	d.sources[field] = *path
	return &text
}
