package plan

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/policy"
)

var symbols = map[Status]string{
	StatusInSync:   "=",
	StatusChange:   "~",
	StatusWaiting:  ".",
	StatusBlocked:  "!",
	StatusRetained: "-",
}

// Render writes the plan for a person: every resource, what would happen to it and why,
// including the ones where nothing happens.
func Render(w io.Writer, p *Plan) {
	fmt.Fprintf(w, "Plan %s\n", short(p.Digest))
	fmt.Fprintf(w, "  revision  %s\n", short(p.Inputs.RevisionDigest))
	fmt.Fprintf(w, "  observed  %s (snapshot %s)\n", p.Inputs.AsOf.UTC().Format(time.RFC3339), short(p.Inputs.SnapshotDigest))
	fmt.Fprintf(w, "  policy    %s\n\n", short(p.Inputs.PolicyDigest))

	for _, c := range p.Changes {
		sym := symbols[c.Status]
		if c.Status == StatusChange && len(c.Diffs) > 0 && allFromEmpty(c.Diffs) {
			sym = "+"
		}
		fmt.Fprintf(w, "%s %s %s  [%s]\n", sym, c.Kind, c.Name, label(c))
		for _, d := range c.Diffs {
			if d.From == "" {
				fmt.Fprintf(w, "      %s: %s\n", d.Field, d.To)
			} else {
				fmt.Fprintf(w, "      %s: %s -> %s\n", d.Field, d.From, d.To)
			}
		}
		for _, op := range c.Operations {
			fmt.Fprintf(w, "    %s: %s (%s %s)\n", op.Op, verdictText(op.Decision.Verdict), op.Decision.Code, op.Decision.Message)
			if op.Decision.Hint != "" {
				fmt.Fprintf(w, "      hint: %s\n", op.Decision.Hint)
			}
		}
		if c.Note != nil {
			fmt.Fprintf(w, "    %s %s\n", c.Note.Code, c.Note.Message)
			if c.Note.Hint != "" {
				fmt.Fprintf(w, "      hint: %s\n", c.Note.Hint)
			}
		}
	}
	fmt.Fprintf(w, "\n%s\n", Summary(p))
	fmt.Fprintln(w, "Nothing has been changed: a plan only describes.")
}

// Summary is a one-line count of the plan.
func Summary(p *Plan) string {
	counts := map[string]int{}
	for _, c := range p.Changes {
		switch c.Status {
		case StatusChange:
			counts[string(c.Verdict)]++
		default:
			counts[string(c.Status)]++
		}
	}
	parts := []string{}
	for _, k := range []string{string(policy.Auto), string(policy.ApprovalRequired), string(policy.Denied),
		string(StatusWaiting), string(StatusBlocked), string(StatusRetained), string(StatusInSync)} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], summaryLabel(k, counts[k])))
		}
	}
	if len(parts) == 0 {
		return "Summary: nothing is declared or observed."
	}
	return "Summary: " + strings.Join(parts, ", ") + "."
}

func label(c Change) string {
	switch c.Status {
	case StatusChange:
		return verdictText(c.Verdict)
	case StatusInSync:
		return "in sync"
	default:
		return string(c.Status)
	}
}

func verdictText(v policy.Verdict) string {
	switch v {
	case policy.Auto:
		return "automatic"
	case policy.ApprovalRequired:
		return "needs approval"
	case policy.Denied:
		return "DENIED"
	}
	return string(v)
}

func summaryLabel(k string, n int) string {
	switch k {
	case string(policy.Auto):
		return "automatic"
	case string(policy.ApprovalRequired):
		if n == 1 {
			return "needs approval"
		}
		return "need approval"
	case string(policy.Denied):
		return "denied"
	case string(StatusInSync):
		return "in sync"
	}
	return k
}

func allFromEmpty(ds []Diff) bool {
	for _, d := range ds {
		if d.From != "" {
			return false
		}
	}
	return true
}

func short(digest string) string {
	if len(digest) > len("sha256:")+12 {
		return digest[:len("sha256:")+12]
	}
	return digest
}
