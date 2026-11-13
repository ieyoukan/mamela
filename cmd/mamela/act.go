package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// runAct carries out one explicit operation of a provider (ADR 0006): deleting, restarting or
// adopting a resource. Explicit operations are never planned. Without --confirm, act only shows
// what it would send; with --confirm <name>, it records the operation, runs the provider's guard,
// sends the change under a permit for exactly it, and runs the guard again.
func runAct(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("mamela act", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	providersFile := addProvidersFlag(fs)
	recoverID := fs.String("recover", "", "re-observe a recorded operation without resending it")
	confirm := fs.String("confirm", "", "the resource's name, typed again: carry the operation out")
	by := fs.String("by", envOr("USER", ""), "who confirms")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela act [flags] <op> <resource> <pome-dir>\n\n"+
			"An explicit operation of a provider (for Proxmox: delete-vm, reboot-vm, reset-vm, adopt-vm): never\n"+
			"planned, carried out only when a person types the resource's name again (--confirm <name>).\n"+
			"<resource> is its name or its ID (\"ProxmoxVM web\"). It acts only on what Mamela recorded creating\n"+
			"(or, to adopt, what the provider reports as adoptable), never on the Control Plane's own host.\n"+
			"Without --confirm, it shows the requests it would send and changes nothing.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	op, target, dir := protocol.Op(fs.Arg(0)), fs.Arg(1), fs.Arg(2)
	site, err := loadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	res, err := t.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	rt, err := startProviders(ctx, dir, t, *providersFile)
	if err != nil {
		return report(stderr, err)
	}
	if rt == nil {
		return report(stderr, errors.New("the declaration uses no provider"))
	}
	defer rt.Close()
	if err := setRecords(ctx, rt, st); err != nil {
		return report(stderr, err)
	}
	if err := rt.CheckGraph(res.Graph); err != nil {
		return report(stderr, err)
	}

	if *recoverID != "" {
		return recoverExplicit(ctx, st, rt, res.Graph, site.Providers().DigestWith(rt.Refs()), *recoverID, string(op), target, stdout, stderr)
	}

	// The resource, as observed now (its ownership vouched for by Mamela's records).
	provided, err := rt.Observe(ctx, res.Graph, wallClock())
	if err != nil {
		return report(stderr, err)
	}
	pr, err := findProvided(provided, target)
	if err != nil {
		return report(stderr, err)
	}
	s, _ := rt.Session(pr.Provider)
	o, ok := s.Manifest().Operation(pr.Kind, op)
	if !ok || !o.Explicit || o.Support != protocol.Supported {
		return report(stderr, fmt.Errorf("MAM-ACT-0101: %s is not an explicit operation of %s %s (it has: %s)", op, pr.Provider, pr.Kind,
			strings.Join(explicitOps(s.Manifest(), pr.Kind), ", ")))
	}
	obs := pr.Observed
	switch {
	case !obs.Exists:
		return report(stderr, fmt.Errorf("MAM-ACT-0102: %s does not exist; nothing to %s", obs.ID, op))
	case o.Adopts && obs.Owned:
		return report(stderr, fmt.Errorf("MAM-ACT-0103: %s is already Mamela's; nothing to adopt", obs.ID))
	case o.Adopts && !obs.Adoptable:
		return report(stderr, fmt.Errorf("MAM-ACT-0104: provider %s does not report %s as adoptable; Mamela adopts only what its provider recognizes as made by Mamela, or what was imported (mamela import terraform)", pr.Provider, obs.ID))
	case !o.Adopts && !obs.Owned:
		return report(stderr, fmt.Errorf("MAM-ACT-0105: Mamela has no record of creating %s (Foreign, or not finished); it acts only on its own resources", obs.ID))
	}
	if site != nil && site.ControlPlane.Host == pr.Name {
		d := policy.ProtectedDecision
		return report(stderr, fmt.Errorf("%s: %s\n  hint: %s", d.Code, d.Message, d.Hint))
	}
	var desired *protocol.Resource
	for _, r := range res.Graph.Resources {
		if r.ID() == obs.ID && r.Provided != nil {
			d := external.Resource(r)
			desired = &d
		}
	}
	change, err := s.Prepare(ctx, op, desired, obs)
	if err != nil {
		return report(stderr, err)
	}
	v := rt.Verify(pr.Provider, change)
	fmt.Fprintf(stdout, "%s %s  via %s\n", op, obs.ID, pr.Provider)
	for _, r := range change.Reasons {
		fmt.Fprintf(stdout, "      %s\n", r)
	}
	fmt.Fprintln(stdout, "    requests:")
	for _, a := range change.Requests {
		fmt.Fprintf(stdout, "      %s %s %s\n", a.Method, a.Path, params(a.Params))
	}
	var effects []string
	for _, e := range v.Effects {
		effects = append(effects, string(e))
	}
	fmt.Fprintf(stdout, "    effects (the host's catalog): %s\n", orNone(strings.Join(effects, ", ")))
	if len(v.Unclassified) > 0 {
		fmt.Fprintf(stdout, "    calls the catalog does not know: %d\n", len(v.Unclassified))
	}
	if *confirm == "" {
		fmt.Fprintf(stdout, "\nNothing was changed. To carry it out: mamela act --confirm %s %s %q %s\n", pr.Name, op, obs.ID, dir)
		return 0
	}
	if *confirm != pr.Name {
		return report(stderr, fmt.Errorf("MAM-ACT-0106: --confirm %q is not the resource's name %q; nothing was changed", *confirm, pr.Name))
	}
	if *by == "" {
		return report(stderr, errors.New("--by: who confirms is required"))
	}

	// The guard first, with this change.
	before := rt.Guard(ctx, protocol.GuardBefore, map[string][]protocol.Change{pr.Provider: {change}}, nil)
	if len(before) > 0 {
		fmt.Fprintln(stdout, "\n== guard (before)")
		printGuard(stdout, before)
		if blocked(before) {
			fmt.Fprintln(stdout, "\nguard: BLOCKED. Nothing was changed.")
			return 1
		}
	}

	defer func() {
		if len(before) > 0 {
			baselines := map[string][]byte{}
			for _, g := range before {
				baselines[g.Provider] = g.Result.Baseline
			}
			after := rt.Guard(ctx, protocol.GuardAfter, nil, baselines)
			fmt.Fprintln(stdout, "\n== guard (after)")
			printGuard(stdout, after)
			if blocked(after) {
				fmt.Fprintln(stdout, "\nguard: BLOCKED after the operation. Something the provider guards changed; investigate.")
				code = 1
			}
		}
	}()

	// Intent first, then the change under a permit for exactly it.
	now := wallClock()
	id := fmt.Sprintf("act-%s-%d", strings.ReplaceAll(string(op), "/", "-"), now.UnixNano())
	if err := st.StartObservedExplicit(ctx, id, pr.Provider, string(op), *by, site.Providers().DigestWith(rt.Refs()), obs, change); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "operation %s\nrecover without resending: mamela act --db %s --site %s --providers %s --recover %s %s %s %s\n", id, shellQuote(*db), shellQuote(*sitePath), shellQuote(*providersFile), id, op, shellQuote(target), shellQuote(dir))
	permit, err := protocol.NewPermit(s.Manifest(), change, id, "act:"+id, site.Providers().DigestWith(rt.Refs()),
		now.Add(time.Hour).Format(time.RFC3339))
	if err != nil {
		return report(stderr, err)
	}
	if err := st.UsePermit(ctx, id, id, "act:"+id); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "\n== %s %s (confirmed by %s)\n", op, obs.ID, *by)
	r := s.Apply(ctx, permit, change, desired, &obs)
	outcome := r.Outcome
	if outcome == host.OutcomeUnknown {
		// Whether it took effect is not known: ask the provider, never send it again blindly.
		if status, _, err := s.Lookup(ctx, change); err == nil && status == protocol.LookupDone {
			outcome = host.OutcomeDone
		}
	}
	detail := ""
	if r.Err != nil {
		detail = r.Err.Error()
	}
	recorded, err := st.Explicit(ctx, id)
	if err != nil {
		return report(stderr, err)
	}
	absent := false
	if outcome == host.OutcomeDone {
		if o.Adopts {
			if err = st.RecordCreation(ctx, pr.Provider, obs.ID, change.IDs, "act:"+id, id); err == nil {
				err = st.ForgetAdoption(ctx, pr.Provider, obs.ID)
			}
		} else {
			absent, err = explicitAbsent(ctx, s, res.Graph, pr.Provider, obs.ID)
		}
		if err != nil {
			outcome, detail = host.OutcomeUnknown, "completion could not be verified: "+err.Error()
		}
	}
	if err := st.CompleteExplicit(ctx, recorded, string(outcome), detail, absent); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "%s\n", outcome)

	switch outcome {
	case host.OutcomeDone:
		return 0
	case host.OutcomeUnknown:
		fmt.Fprintf(stderr, "MAM-ACT-0201: whether %s took effect is not known: %s\n  hint: use --recover %s to verify the original operation without resending it\n", op, detail, id)
	default:
		fmt.Fprintf(stderr, "MAM-ACT-0202: %s %s: %v\n", op, outcome, r.Err)
	}
	return 1
}

// findProvided finds a provider's resource by its ID or its name (which must name one resource).
func findProvided(provided map[string]state.Provided, target string) (state.Provided, error) {
	if p, ok := provided[target]; ok {
		return p, nil
	}
	var found []state.Provided
	for _, id := range slices.Sorted(maps.Keys(provided)) {
		if provided[id].Name == target {
			found = append(found, provided[id])
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return state.Provided{}, fmt.Errorf("MAM-ACT-0100: no resource of a provider is named %q (declared, or created by Mamela and still there)", target)
	}
	var ids []string
	for _, p := range found {
		ids = append(ids, fmt.Sprintf("%q", p.Observed.ID))
	}
	return state.Provided{}, fmt.Errorf("MAM-ACT-0100: %q names several resources (%s); give the ID", target, strings.Join(ids, ", "))
}

func explicitOps(m protocol.Manifest, kind protocol.ResourceKind) []string {
	var out []string
	for _, k := range m.Kinds {
		if k.Kind != kind {
			continue
		}
		for _, o := range k.Operations {
			if o.Explicit && o.Support == protocol.Supported {
				out = append(out, string(o.Op))
			}
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func params(p map[string][]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(p)) {
		for _, v := range p[k] {
			if len(v) > 60 {
				v = v[:57] + "..."
			}
			out = append(out, k+"="+strings.ReplaceAll(v, "\n", `\n`))
		}
	}
	return strings.Join(out, " ")
}

// explicitAbsent uses the provider's read-only inventory, which includes its undeclared owned
// resources. Data loss alone never implies deletion: an existing target keeps its ownership.
func explicitAbsent(ctx context.Context, session *host.Session, g *graph.Graph, provider, id string) (bool, error) {
	var resources []protocol.Resource
	declared := false
	for _, r := range g.Sorted() {
		if r.Provided != nil && r.Provided.Provider == provider {
			resources = append(resources, external.Resource(r))
			declared = declared || r.ID() == id
		}
	}
	observed, _, err := session.ObserveWith(ctx, protocol.ObserveParams{Resources: resources})
	if err != nil {
		return false, err
	}
	for _, o := range observed {
		if o.ID == id {
			return !o.Exists, nil
		}
	}
	if declared {
		return false, fmt.Errorf("provider omitted declared resource %s; absence is unconfirmed", id)
	}
	return true, nil
}

func recoverExplicit(ctx context.Context, st *store.Store, rt *external.Runtime, g *graph.Graph, providers, id, op, target string, stdout, stderr io.Writer) int {
	e, err := st.Explicit(ctx, id)
	if err != nil {
		return report(stderr, err)
	}
	if e.Op != op || target != e.Resource && target != e.Context.Observed.Name || e.Context.Providers != providers {
		return report(stderr, fmt.Errorf("MAM-ACT-0204: recovery must use the original operation, resource and provider contract"))
	}
	s, ok := rt.Session(e.Provider)
	if !ok {
		return report(stderr, fmt.Errorf("provider %s is unavailable", e.Provider))
	}
	status, _, err := s.Lookup(ctx, e.Change)
	if err != nil || status != protocol.LookupDone {
		return report(stderr, fmt.Errorf("MAM-ACT-0201: operation %s remains %s (%v); no requests were resent", id, status, err))
	}
	absent, err := explicitAbsent(ctx, s, g, e.Provider, e.Resource)
	if err != nil {
		return report(stderr, err)
	}
	o, _ := s.Manifest().Operation(e.Context.Observed.Kind, protocol.Op(e.Op))
	if o.Adopts {
		if absent {
			return report(stderr, fmt.Errorf("adopted resource is absent; inspect before recovering"))
		}
		if err = st.RecordCreation(ctx, e.Provider, e.Resource, e.Change.IDs, "act:"+id, id); err != nil {
			return report(stderr, err)
		}
		if err = st.ForgetAdoption(ctx, e.Provider, e.Resource); err != nil {
			return report(stderr, err)
		}
	}
	if err := st.CompleteExplicit(ctx, e, string(host.OutcomeDone), "confirmed by read-only recovery", absent); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "operation %s confirmed by observation; no requests were resent\n", id)
	return 0
}
