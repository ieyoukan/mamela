package evaluate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

// A declaration may leave the sha256 of what a node downloads out (Mamela.artifact_pinned,
// K3s.server_pinned): it is then the one the declaration's mamela.sum has for the URL, read from
// the root of the Pome and given to the decoder with the Pome's files (graph.Files.Sum, as
// contentFrom's texts are) before the graph is validated (docs/design/declaration-inputs.md, 2). The
// graph and its digest are the same as when the declaration writes the sha256 itself. Evaluating
// reads the file only: `mamela pin` downloads and writes it.

// Sum is the sha256 the Pome's mamela.sum has for url (graph.Files).
func (f *pomeFiles) Sum(url string) (string, bool) { return f.sums.Lookup(url) }

// readSums reads the mamela.sum of the Pome in dir (none: no sha256 at all). It is read before the
// build, so that a file that is not the tool's stops the evaluation at once.
func readSums(dir string) (*sumfile.Sums, error) {
	s, _, err := sumfile.Read(dir)
	if err == nil {
		return s, nil
	}
	var se *sumfile.Error
	if !errors.As(err, &se) {
		return nil, &Error{Code: CodeSumMalformed, Stage: StageSums, Message: fmt.Sprintf("%s cannot be read: %s", sumfile.Name, reason(err)),
			Hint: "check the file's permissions"}
	}
	return nil, &Error{Code: CodeSumMalformed, Stage: StageSums,
		Message: se.Error() + "; nothing was evaluated",
		Hint: fmt.Sprintf("%s is written by `mamela pin`, not by hand: take it back from git (git checkout -- %s) and run `mamela pin %s`, "+
			"or remove it and run `mamela pin %s` to write it again (which downloads everything again: compare what it adds with the vendors' checksums)",
			sumfile.Name, sumfile.Name, dir, dir)}
}

// missingSums is the evaluation error of a declaration whose mamela.sum lacks some sha256.
func missingSums(dir string, m *graph.MissingSums) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has no sha256 for %d URL%s the declaration downloads without naming its sha256; nothing is downloaded while evaluating:",
		sumfile.Name, len(m.URLs()), plural(len(m.URLs())))
	for _, p := range m.Pins {
		fmt.Fprintf(&b, "\n  %s (%s %s)", p.URL, p.Resource, p.Field)
	}
	return &Error{Code: CodeSumMissing, Stage: StageSums, Message: b.String(),
		Hint: fmt.Sprintf("run `mamela pin %s`: it downloads each URL once, writes its sha256 into %s and shows what it added. "+
			"Check that, and commit the file with the declaration", dir, sumfile.Name)}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Pins evaluates the Pome in dir without its mamela.sum, and returns the sha256 its declaration
// leaves to mamela.sum, in the order of the resources (`mamela pin`). The graph is validated first,
// with a stand-in for each of them: a declaration Mamela refuses has nothing to pin.
func (e *Evaluator) Pins(ctx context.Context, dir string) ([]graph.Pin, error) {
	_, err := e.evaluate(ctx, dir, nil)
	var missing *graph.MissingSums
	switch {
	case errors.As(err, &missing):
		return missing.Pins, nil
	case err != nil:
		return nil, err
	}
	return nil, nil
}
