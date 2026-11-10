// Command mamela is the Mamela CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/ieyoukan/mamela/internal/evaluate"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
)

const usage = `mamela - safe, declarative infrastructure (early development)

Usage:
  mamela eval [flags] <pome-dir>      evaluate an Emela declaration into a Resource Graph (read-only)
  mamela check [--json] <pome-dir>    list what each resource can need (capability, target) and any problems
  mamela plan [flags] <pome-dir>      show and save what would change, and what needs approval
  mamela plan --replay <plan-digest>  recompute a saved plan and check it is unchanged
  mamela approve <plan-digest>        approve exactly that plan
  mamela apply --site <file> <plan-digest>  carry out an approved plan on the lab Proxmox
  mamela destroy --site <file> <vm>   destroy a VM Mamela created (shows it first; needs --confirm <vm>)
  mamela serve --site <file>          serve mamelet (the agent in lab VMs): enrollment, revisions, reports
  mamela lab <bootstrap|preflight|postflight|tunnel>  set up and check the live test lab (docs/lab.md)
  mamela provider <inspect|trust>     check a provider program; trust a native one
  mamela toolchain [status|install]  check or install the pinned emela and wasmtime
  mamela version                      print the Mamela version and its pinned toolchain

Run 'mamela <command> -h' for flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "eval":
		return runEval(ctx, args[1:], stdout, stderr)
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr)
	case "approve":
		return runApprove(ctx, args[1:], stdout, stderr)
	case "apply":
		return runApply(ctx, args[1:], stdout, stderr)
	case "lab":
		return runLab(ctx, args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "destroy":
		return runDestroy(ctx, args[1:], stdout, stderr)
	case "provider":
		return runProvider(ctx, args[1:], stdout, stderr)
	case "toolchain":
		return runToolchain(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		return runVersion(stdout)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mamela: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the canonical mamela-graph/v1 JSON instead of a summary")
	sitePath := fs.String("site", "", "lab site definition, to name the ACL paths in what the declaration may need")
	twice := fs.Bool("twice", false, "evaluate twice and require the same digest (determinism check)")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela eval [flags] <pome-dir>\n\n"+
			"Builds the Pome to WASM, checks its imports, runs it in a sandbox and validates the graph.\n"+
			"Nothing outside the sandbox is changed.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)

	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	e := t.evaluator()
	var res *evaluate.Result
	if *twice {
		res, err = e.EvaluateTwice(ctx, dir)
	} else {
		res, err = e.Evaluate(ctx, dir)
	}
	if err != nil {
		return report(stderr, err)
	}

	if *asJSON {
		fmt.Fprintf(stdout, "%s\n", res.Graph.Canonical())
		return 0
	}
	printSummary(stdout, dir, res, *twice)
	var site *lab.Site // nil: placeholder paths
	if *sitePath != "" {
		if site, err = lab.LoadSite(*sitePath); err != nil {
			return report(stderr, err)
		}
	}
	ps, err := declaredProviders(ctx, site, dir, t)
	if err != nil {
		return report(stderr, err)
	}
	renderMayNeed(stdout, site, ps, res.Graph)
	if s := strings.TrimSpace(res.Stderr); s != "" {
		fmt.Fprintf(stderr, "diagnostics from the declaration:\n%s\n", indent(s, "  "))
	}
	return 0
}

func printSummary(w io.Writer, dir string, res *evaluate.Result, twice bool) {
	n := len(res.Graph.Resources)
	fmt.Fprintf(w, "%s: %d resource%s\n", dir, n, plural(n))
	names := make([]string, len(res.Imports))
	for i, im := range res.Imports {
		names[i] = im.Name
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  graph\t%s\n", res.Digest)
	if twice {
		fmt.Fprintf(tw, "  \t(evaluated twice, same digest)\n")
	}
	fmt.Fprintf(tw, "  wasm\t%s\n", res.WasmDigest)
	fmt.Fprintf(tw, "  toolchain\temela %s, wasmtime %s\n", res.Versions.Emela, res.Versions.Wasmtime)
	fmt.Fprintf(tw, "  imports\t%s (all allowed)\n", strings.Join(names, ", "))
	tw.Flush()
	if n == 0 {
		fmt.Fprintln(w, "\n  nothing is declared")
		return
	}
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range res.Graph.Sorted() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Kind, r.Name, describe(r))
	}
	tw.Flush()
}

func describe(r graph.Resource) string {
	switch r.Kind {
	case graph.KindVirtualMachine:
		s := r.VirtualMachine
		return fmt.Sprintf("%d vCPU, %d MiB memory, %d GiB disk, image %s, network %s", s.CPU, s.MemoryMiB, s.DiskGiB, s.Image, s.Network)
	case graph.KindNodePackage:
		s := r.NodePackage
		return fmt.Sprintf("package %s present on %s", s.Package, s.Node)
	case graph.KindNodeService:
		s := r.NodeService
		return fmt.Sprintf("service %s %s, %s on %s", s.Service,
			choose(s.Enabled, "enabled", "disabled"), choose(s.Running, "running", "stopped"), s.Node)
	}
	if k := r.NodeK3s; k != nil {
		return fmt.Sprintf("k3s %s (sha256 %s…) on %s", k.Version, k.SHA256[:12], k.Node)
	}
	if p := r.Provided; p != nil {
		return fmt.Sprintf("via provider %s: %s", p.Provider, p.Spec)
	}
	return ""
}

// report prints an error for a person and returns the exit code: 1 when the declaration was
// rejected or could not be evaluated.
func report(w io.Writer, err error) int {
	var ge *graph.Error
	var ee *evaluate.Error
	switch {
	case errors.As(err, &ge):
		fmt.Fprintf(w, "the declaration produced an invalid graph (%d problem%s):\n\n", len(ge.Problems), plural(len(ge.Problems)))
		for _, p := range ge.Problems {
			fmt.Fprintf(w, "%s\n\n", indent(p.String(), "  "))
		}
	case errors.As(err, &ee):
		fmt.Fprintf(w, "evaluation failed at the %s step:\n\n%s\n", ee.Stage, indent(ee.Error(), "  "))
	default:
		fmt.Fprintf(w, "mamela: %v\n", err)
	}
	return 1
}

func defaultPomeCache() string {
	if v := os.Getenv("EMELA_POME_CACHE"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".emela", "pomes")
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func choose(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
