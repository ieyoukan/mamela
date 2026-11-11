package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const defaultDB = ".mamela/state.db"

// wallClock is the only place the CLI reads the current time; planning itself never does.
var wallClock = func() time.Time { return time.Now().UTC() }

func openStore(ctx context.Context, path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return store.Open(ctx, path, wallClock)
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	snapshotFile := fs.String("snapshot", "", "observed state as a snapshot JSON file (default: an empty world, observed now)")
	sitePath := fs.String("site", "", "observe the lab Proxmox of this site definition instead (read-only)")
	replay := fs.String("replay", "", "recompute a stored plan by digest and check that it is unchanged")
	tf := addToolFlags(fs)
	policyFile := fs.String("policy", "", "policy JSON file (default: the built-in conservative policy)")
	providersFile := addProvidersFlag(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela plan [flags] <pome-dir>\n       mamela plan --replay <plan-digest>\n\n"+
			"Shows what would change, why, and what needs approval. Saves the plan so it can be approved.\n"+
			"Nothing outside the state database is changed. With --site, it also updates what each node's\n"+
			"mamelet observes (never what it may change: that takes an approved apply).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()

	if *replay != "" {
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		p, _, err := st.LoadPlan(ctx, *replay)
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "no plan %s in %s\n", *replay, *db)
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "MAM-REC-0001: %v\n  hint: the stored plan or its inputs changed; do not apply it, make a new plan\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "plan %s recomputes to the same digest from its stored inputs\n\n", p.Digest)
		plan.Render(stdout, p)
		return 0
	}

	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	res, err := t.evaluator().Evaluate(ctx, fs.Arg(0))
	if err != nil {
		return report(stderr, err)
	}
	// The providers the declaration imports (its Pome's dependencies), running isolated.
	rt, err := startProviders(ctx, fs.Arg(0), t, *providersFile)
	if err != nil {
		return report(stderr, err)
	}
	defer rt.Close()
	if err := rt.CheckGraph(res.Graph); err != nil {
		return report(stderr, err)
	}

	snap := state.Empty(wallClock())
	if *snapshotFile != "" && *sitePath != "" {
		fmt.Fprintln(stderr, "mamela plan: use either --snapshot or --site, not both")
		return 2
	}
	var site *lab.Site
	if *sitePath != "" {
		if site, err = lab.LoadSite(*sitePath); err != nil {
			return report(stderr, err)
		}
		operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
		if err != nil {
			return report(stderr, err)
		}
		prov, err := labProvider(site, operating, st, "", false)
		if err != nil {
			return report(stderr, err)
		}
		if snap, err = prov.Observe(ctx); err != nil {
			return report(stderr, fmt.Errorf("observe the lab: %w", err))
		}
	}
	if *snapshotFile != "" {
		b, err := os.ReadFile(*snapshotFile)
		if err != nil {
			return report(stderr, err)
		}
		if snap, err = state.Decode(b); err != nil {
			return report(stderr, err)
		}
	}
	pol := policy.Default
	if *policyFile != "" {
		b, err := os.ReadFile(*policyFile)
		if err != nil {
			return report(stderr, err)
		}
		if pol, err = policy.Decode(b); err != nil {
			return report(stderr, fmt.Errorf("%s: %w", *policyFile, err))
		}
	}
	if site != nil && site.ControlPlane.Host != "" {
		// This Control Plane's own host: shown, never changed (ADR 0003). The policy is stored with
		// the plan, so apply derives the same denial.
		pol.ProtectedNodes = append(slices.Clone(pol.ProtectedNodes), site.ControlPlane.Host)
	}
	provided, err := rt.Observe(ctx, res.Graph, snap.AsOf)
	if err != nil {
		return report(stderr, err)
	}
	snap.AddProvided(provided)
	p := plan.BuildFor(res.Graph, snap, pol, site.Providers().DigestWith(rt.Refs()))
	if site != nil {
		// A VM the site would refuse to create is refused here, before anyone approves it.
		if refused := siteRefusals(site, p, res.Graph); len(refused) > 0 {
			for _, r := range refused {
				fmt.Fprintf(stderr, "LAB-0304: %s\n", r)
			}
			fmt.Fprintf(stderr, "  hint: declare an address in the site's network.static_first..static_last, or Dhcp if network.dhcp allows it (%s)\n", *sitePath)
			return 1
		}
	}
	if err := st.SavePlan(ctx, res.Graph, snap, pol, p); err != nil {
		return report(stderr, err)
	}
	// The programs the plan was made with: apply runs exactly these.
	if err := st.SavePlanPrograms(ctx, p.Digest, rt.Programs()); err != nil {
		return report(stderr, err)
	}
	if *sitePath != "" {
		// Tell each node's mamelet what to observe (and the repairs the policy allows by itself).
		// Revisions name a Control Plane only to move mamelet there: a mamelet older than ADR 0003
		// refuses a revision with a member it does not know.
		ids := map[string]int64{}
		for name, n := range snap.Nodes {
			if n.Managed {
				ids[name] = n.ID
			}
		}
		if err := node.Publish(ctx, st, res.Graph, pol, site.ControlPlane.MoveTo, ids); err != nil {
			return report(stderr, err)
		}
	}
	plan.Render(stdout, p)
	if site != nil {
		renderPlanUses(stdout, site, p, res.Graph, pol)
	}
	fmt.Fprintf(stdout, "\nSaved to %s. To approve: mamela approve %s\n", *db, p.Digest)
	return 0
}

func runApprove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	by := fs.String("by", envOr("USER", ""), "who approves")
	ttl := fs.Duration("ttl", time.Hour, "how long the approval stays valid")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela approve [flags] <plan-digest>\n\n"+
			"Approves exactly this plan. Any change to the declaration, the observation or the policy makes a\n"+
			"different plan, which needs its own approval. Denied operations never run, approved or not.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *by == "" || *ttl <= 0 {
		fs.Usage()
		return 2
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	p, _, err := st.LoadPlan(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "cannot approve %s: %v\n", fs.Arg(0), err)
		return 1
	}
	a, err := st.Approve(ctx, p.Digest, *by, *ttl)
	if err != nil {
		return report(stderr, err)
	}
	denied := 0
	for _, c := range p.Changes {
		if c.Verdict == policy.Denied {
			denied++
		}
	}
	fmt.Fprintf(stdout, "approved %s by %s until %s\n", p.Digest, a.Approver, a.ExpiresAt.Format(time.RFC3339))
	if denied > 0 {
		fmt.Fprintf(stdout, "note: %d change%s in this plan %s denied and will not run\n", denied, plural(denied), choose(denied == 1, "is", "are"))
	}
	return 0
}

// siteRefusals says which VMs the plan would create with a network the site does not allow: built-in
// VMs, and providers' machines whose seed names a network (ADR 0004).
func siteRefusals(site *lab.Site, p *plan.Plan, g *graph.Graph) []string {
	var out []string
	for _, c := range p.Changes {
		for _, pc := range c.ProviderChanges {
			for _, a := range pc.Artifacts {
				if a.Kind == protocol.ArtifactSeed && a.Network != nil {
					if err := site.Allows(a.Node, graphNetwork(*a.Network)); err != nil {
						out = append(out, err.Error())
					}
				}
			}
		}
		if c.Kind != graph.KindVirtualMachine || !slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == policy.OpCreateVM }) {
			continue
		}
		for _, r := range g.Resources {
			if r.Kind == graph.KindVirtualMachine && r.Name == c.Name {
				if err := site.Allows(r.Name, r.VirtualMachine.Network); err != nil {
					out = append(out, err.Error())
				}
			}
		}
	}
	return out
}
