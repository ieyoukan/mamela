package plan

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/safetext"
)

var symbols = map[Status]string{
	StatusInSync:   "=",
	StatusChange:   "~",
	StatusWaiting:  ".",
	StatusBlocked:  "!",
	StatusRetained: "=",
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
		// "+" is a create. A provider's diffs are reasons (no "from"): it creates when its object
		// does not exist yet.
		if c.Removes || (c.Deletes() && !slices.ContainsFunc(c.Operations, func(o Operation) bool { return !o.Deletes })) {
			sym = "-"
		} else if c.Status == StatusChange && (c.Creates || c.Provider == "" && len(c.Diffs) > 0 && allFromEmpty(c.Diffs)) {
			sym = "+"
		}
		via := ""
		if c.Provider != "" {
			via = "  via " + esc(c.Provider)
		}
		fmt.Fprintf(w, "%s %s %s  [%s]%s\n", sym, esc(string(c.Kind)), esc(c.Name), label(c), via)
		for _, d := range c.Diffs {
			renderDiff(w, d)
		}
		for _, op := range c.Operations {
			fmt.Fprintf(w, "    %s: %s (%s %s)\n", esc(string(op.Op)), verdictText(op.Decision.Verdict), esc(string(op.Decision.Code)), esc(op.Decision.Message))
			if op.Decision.Hint != "" {
				fmt.Fprintf(w, "      hint: %s\n", esc(op.Decision.Hint))
			}
			// What approving approves, exactly: the objects and every request with its parameters.
			for _, pc := range c.ProviderChanges {
				if pc.Op != op.Op {
					continue
				}
				fmt.Fprintf(w, "      objects: %s\n", formatMap(pc.IDs))
				if pc.Precondition != "" {
					fmt.Fprintf(w, "      based on version %s\n", esc(pc.Precondition))
				}
				for _, r := range pc.Requests {
					fmt.Fprintf(w, "      sends %s %s %s\n", esc(r.Method), esc(r.Path), formatParams(r.Params))
				}
			}
		}
		for _, n := range c.Warnings {
			fmt.Fprintf(w, "    warning %s %s\n", esc(n.Code), esc(n.Message))
			if n.Hint != "" {
				fmt.Fprintf(w, "      hint: %s\n", esc(n.Hint))
			}
		}
		if c.Note != nil {
			fmt.Fprintf(w, "    %s %s\n", esc(c.Note.Code), esc(c.Note.Message))
			if c.Note.Hint != "" {
				fmt.Fprintf(w, "      hint: %s\n", esc(c.Note.Hint))
			}
		}
	}
	fmt.Fprintf(w, "\n%s\n", Summary(p))
	fmt.Fprintln(w, "Nothing has been changed: a plan only describes.")
}

// esc shows what a plan quotes (a name, a value, a request, a provider's words) so that it cannot
// change how the plan looks: a control character, a bidirectional override or a zero-width
// character is written out (\x1b, \u202e), not sent to the terminal (A9 of the 0.3.2 audit).
func esc(s string) string { return safetext.Escape(s) }

// renderDiff writes one field that differs. A value of one line is "field: from -> to"; a text of
// several lines (the content of a manifest, which is approved as it is shown) is written line by
// line, each line after a marker of its own ("+" is what it will be, "-" what it is), so that no
// line of it can pass for a line of the plan.
func renderDiff(w io.Writer, d Diff) {
	if !strings.Contains(d.From, "\n") && !strings.Contains(d.To, "\n") {
		if d.From == "" {
			fmt.Fprintf(w, "      %s: %s\n", esc(d.Field), esc(d.To))
		} else {
			fmt.Fprintf(w, "      %s: %s -> %s\n", esc(d.Field), esc(d.From), esc(d.To))
		}
		return
	}
	fmt.Fprintf(w, "      %s:\n", esc(d.Field))
	if d.From != "" {
		for _, l := range safetext.EscapeLines(d.From) {
			fmt.Fprintf(w, "      - %s\n", l)
		}
	}
	for _, l := range safetext.EscapeLines(d.To) {
		fmt.Fprintf(w, "      + %s\n", l)
	}
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
		return esc(string(c.Status))
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
	return esc(string(v))
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

func formatMap(m map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, esc(k)+"="+esc(m[k]))
	}
	return strings.Join(out, " ")
}

func formatParams(ps map[string][]string) string {
	if len(ps) == 0 {
		return "(no parameters)"
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(ps)) {
		vs := make([]string, len(ps[k]))
		for i, v := range ps[k] {
			vs[i] = esc(v)
		}
		out = append(out, esc(k)+"="+strings.Join(vs, ","))
	}
	return strings.Join(out, " ")
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
