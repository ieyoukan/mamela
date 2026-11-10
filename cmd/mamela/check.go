package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ieyoukan/mamela/internal/evaluate"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// CheckSchema names the format of `mamela check --json`.
const CheckSchema = "mamela-check/v1"

// CheckResult is what `mamela check --json` prints: everything an editor or a tool needs to show
// what is wrong with a declaration and what it requires, as data.
type CheckResult struct {
	Schema       string             `json:"schema"`
	Pome         string             `json:"pome"`
	Graph        string             `json:"graph,omitempty"` // digest; empty when evaluation failed
	Providers    []ProviderRef      `json:"providers"`
	Diagnostics  []Diagnostic       `json:"diagnostics"`
	Observation  []spec.Requirement `json:"observation"`
	Requirements []spec.Requirement `json:"requirements"`
}

// ProviderRef is a provider and the version of its contract.
type ProviderRef struct {
	Provider string `json:"provider"`
	Version  string `json:"version"`
}

// Diagnostic is one problem, in the shape editors use (severity, code, message, where, hint).
type Diagnostic struct {
	Severity string `json:"severity"` // error, warning
	Code     string `json:"code"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"` // the resource or field it is about, when known
	Hint     string `json:"hint,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print a mamela-check/v1 JSON document (for editors and tools)")
	sitePath := fs.String("site", "", "lab site definition, to resolve targets to the site's names (read only; nothing is contacted)")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela check [flags] <pome-dir>\n\n"+
			"Evaluates the declaration and lists, for every resource, every operation it can lead to and the\n"+
			"capabilities each needs (provider, permission, target), with any problems. Read-only.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	var site *lab.Site
	if *sitePath != "" {
		s, err := lab.LoadSite(*sitePath)
		if err != nil {
			return report(stderr, err)
		}
		site = s
	}
	out := CheckResult{Schema: CheckSchema, Pome: fs.Arg(0), Diagnostics: []Diagnostic{}}
	ps := site.Providers()
	t, err := tf.find()
	var res *evaluate.Result
	if err == nil {
		res, err = t.evaluator().Evaluate(ctx, fs.Arg(0))
	}
	if err == nil {
		ps, err = declaredProviders(ctx, site, fs.Arg(0), t)
	}
	for _, m := range ps {
		out.Providers = append(out.Providers, ProviderRef{m.Provider, m.Version})
	}
	if err != nil {
		out.Diagnostics = diagnostics(err)
	} else {
		out.Graph = res.Digest
		out.Observation = ps.Observation(site)
		out.Requirements = ps.Analyze(res.Graph, site)
	}
	if !*asJSON {
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "%s: %d resources, graph %s\n", out.Pome, len(res.Graph.Resources), out.Graph)
		renderMayNeed(stdout, site, ps, res.Graph)
		return 0
	}
	data, merr := json.Marshal(out, json.Deterministic(true))
	if merr != nil {
		return report(stderr, merr)
	}
	fmt.Fprintf(stdout, "%s\n", data)
	if len(out.Diagnostics) > 0 {
		return 1
	}
	return 0
}

// diagnostics turns an evaluation failure into diagnostics.
func diagnostics(err error) []Diagnostic {
	var ge *graph.Error
	var ee *evaluate.Error
	switch {
	case errors.As(err, &ge):
		var out []Diagnostic
		for _, p := range ge.Problems {
			out = append(out, Diagnostic{Severity: "error", Code: string(p.Code), Message: p.Message, Resource: p.Path, Hint: p.Hint})
		}
		return out
	case errors.As(err, &ee):
		return []Diagnostic{{Severity: "error", Code: string(ee.Code), Message: ee.Message, Hint: ee.Hint, Detail: ee.Detail}}
	default:
		return []Diagnostic{{Severity: "error", Code: "MAM-CLI-0001", Message: err.Error()}}
	}
}
