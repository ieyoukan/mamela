package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/seed"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The three operations of a Control Plane: plan a declaration, approve a plan, apply an approved
// plan. `mamela plan|approve|apply` and `mamela api` (the backend in api.go) both call these, so
// they do the same thing; the commands only parse flags and print.

// told is an error whose text is already the whole message for a person (what, why, what to do).
// The CLI prints it as it is, without the "mamela: " prefix.
type told struct{ text string }

func (e told) Error() string { return e.text }

// rendered is what the CLI would print for err, for callers that return it as text (the API).
func rendered(err error) string {
	var b strings.Builder
	report(&b, err)
	return strings.TrimSpace(b.String())
}

// planOptions say where a plan reads from. Tools is called when the toolchain is first needed.
type planOptions struct {
	SitePath      string
	ProvidersFile string
	PolicyFile    string // "": the built-in policy
	SnapshotFile  string // "": the observation of the world, now
	Tools         func() (tools, error)
}

// planned is a saved plan with what was used to make it.
type planned struct {
	Plan      *plan.Plan
	Graph     *graph.Graph
	Policy    policy.Policy
	Site      *sitedef.Site // nil: no site
	Programs  []store.Program
	Manifests spec.Providers
	Warnings  []string // for a person to read; the plan is saved regardless
}

// planDeclaration evaluates the declaration in dir, plans it against the observed world and saves
// the plan (so that it can be approved). Nothing outside the state database is changed; with a
// Control Plane in the site it also updates what each node's mamelet observes, never what it may
// change.
func planDeclaration(ctx context.Context, st *store.Store, dir string, o planOptions) (*planned, error) {
	t, err := o.Tools()
	if err != nil {
		return nil, err
	}
	res, err := t.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return nil, err
	}
	// The providers the declaration imports (its Pome's dependencies), running isolated.
	rt, err := startProviders(ctx, dir, t, o.ProvidersFile)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	if err := setRecords(ctx, rt, st); err != nil {
		return nil, err
	}
	// The declaration's name: what is created is recorded for it, and only what it made is deleted
	// when it no longer has it (ADR 0013).
	rt.SetStack(res.Stack)
	// What the other plans of the last hour are to create is taken: plans made one after another
	// and applied together do not choose the same IDs (a VMID).
	claims, err := st.Claims(ctx, wallClock().Add(-claimWindow), "")
	if err != nil {
		return nil, err
	}
	rt.SetClaims(claims)
	if err := rt.CheckGraph(res.Graph); err != nil {
		return nil, err
	}

	site, err := loadSite(o.SitePath)
	if err != nil {
		return nil, err
	}
	// What mamelet on each machine last reported (read from the database; nothing is contacted).
	snap, err := (&node.Provider{Store: st, Clock: wallClock}).Observe(ctx)
	if err != nil {
		return nil, err
	}
	if o.SnapshotFile != "" {
		b, err := os.ReadFile(o.SnapshotFile)
		if err != nil {
			return nil, err
		}
		if snap, err = state.Decode(b); err != nil {
			return nil, err
		}
	}
	pol := policy.Default
	if o.PolicyFile != "" {
		b, err := os.ReadFile(o.PolicyFile)
		if err != nil {
			return nil, err
		}
		if pol, err = policy.Decode(b); err != nil {
			return nil, fmt.Errorf("%s: %w", o.PolicyFile, err)
		}
	}
	if site != nil && site.ControlPlane.Host != "" {
		// This Control Plane's own host: shown, never changed (ADR 0003). The policy is stored with
		// the plan, so apply derives the same denial.
		pol.ProtectedNodes = append(slices.Clone(pol.ProtectedNodes), site.ControlPlane.Host)
	}
	rt.SetLimits(siteLimits(site))
	provided, err := rt.Observe(ctx, res.Graph, snap.AsOf)
	if err != nil {
		return nil, err
	}
	snap.Stack = res.Stack
	snap.AddProvided(provided)
	p := plan.BuildFor(res.Graph, snap, pol, site.Providers().DigestWith(rt.Refs()))
	if site != nil {
		// A machine the site would refuse to create is refused here, before anyone approves it.
		if refused := siteRefusals(site, p); len(refused) > 0 {
			var lines []string
			for _, r := range refused {
				lines = append(lines, "MAM-SITE-0304: "+r)
			}
			lines = append(lines, fmt.Sprintf("  hint: declare an address in the site's network.static_first..static_last, or Dhcp if network.dhcp allows it (%s)", o.SitePath))
			return nil, told{strings.Join(lines, "\n")}
		}
	}
	if err := st.SavePlan(ctx, res.Graph, snap, pol, p); err != nil {
		return nil, err
	}
	// The programs the plan was made with: apply runs exactly these.
	if err := st.SavePlanPrograms(ctx, p.Digest, rt.Programs()); err != nil {
		return nil, err
	}
	out := &planned{Plan: p, Graph: res.Graph, Policy: pol, Site: site, Programs: rt.Programs(), Manifests: rt.Manifests(),
		Warnings: rt.Warnings()} // a provider that is not pinned (MAM-PRV-0010, ADR 0015)
	if site != nil && site.ControlPlane.Enabled() {
		if w := mameletServedWarning(site, res.Graph); w != "" {
			out.Warnings = append(out.Warnings, w)
		}
		if ws, err := agentCertWarnings(ctx, st, wallClock()); err == nil {
			out.Warnings = append(out.Warnings, ws...) // certificates of mamelets that ended or end soon (MAM-PKI-0101, 0102)
		}
		if site.ControlPlane.MoveTo != "" {
			out.Warnings = append(out.Warnings, "MAM-NODE-0102: plan does not relocate agents; use mamela controlplane redirect --db <database> --url <new-endpoint> after the new Control Plane is ready")
		}
		// Observation targets only. Active values and grants are given by apply, never plan; what the
		// declaration no longer has loses its permissions here (node.Publish), which only takes away.
		ids := map[string]int64{}
		for name, n := range snap.Nodes {
			if n.Managed {
				ids[name] = n.ID
			}
		}
		if err := node.Publish(ctx, st, res.Graph, pol, site.ControlPlane.MoveTo, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// approved is what an approval recorded.
type approved struct {
	Approval store.Approval
	Denied   int      // changes of the plan that will not run, approved or not
	Removed  []string // the objects the plan deletes
}

// approvePlan approves exactly the saved plan with this digest. Any change to the declaration, the
// observation or the policy makes a different plan, which needs its own approval. A plan that
// deletes objects (ADR 0013) is approved only on the Control Plane's host (host): what reaches the
// local API comes from GitHub's side, which must not be able to delete anything.
func approvePlan(ctx context.Context, st *store.Store, digest, by string, ttl time.Duration, host bool) (approved, error) {
	p, _, err := st.LoadPlan(ctx, digest)
	if err != nil {
		return approved{}, told{fmt.Sprintf("cannot approve %s: %v", digest, err)}
	}
	out := approved{}
	for _, c := range p.Changes {
		if c.Verdict == policy.Denied {
			out.Denied++
		}
		if c.Deletes() {
			out.Removed = append(out.Removed, c.Deleted()...)
		}
	}
	if len(out.Removed) > 0 && !host {
		return approved{}, fmt.Errorf("%w: plan %s deletes %s", api.ErrNeedsHost, digest, strings.Join(out.Removed, ", "))
	}
	if out.Approval, err = st.Approve(ctx, p.Digest, by, ttl); err != nil {
		return approved{}, err
	}
	return out, nil
}

// noPlanError: the database has no plan with this digest.
type noPlanError struct{ Digest string }

func (e *noPlanError) Error() string { return "no plan " + e.Digest }

// applyOptions say what an apply reads besides the plan. Tools is called only when the plan
// has providers' programs to run.
type applyOptions struct {
	ProvidersFile string
	ImageCache    string
	Tools         func() (tools, error)
}

// applyResult says how an apply that did not fail ended. The zero value is a clean finish.
type applyResult struct {
	BlockedBefore bool            // a guard blocked: nothing was sent
	BlockedAfter  bool            // a guard blocked after the apply: investigate
	Stopped       *reconcile.Stop // the reconciler stopped early
}

// OK reports that the plan was carried out and the guards were clear.
func (r applyResult) OK() bool { return !r.BlockedBefore && !r.BlockedAfter && r.Stopped == nil }

// Stale reports that the world was not what the plan assumed: the declaration is to be planned again.
func (r applyResult) Stale() bool { return r.Stopped != nil && r.Stopped.Code == reconcile.CodeStale }

// applyPlan carries out an approved plan through the providers the plan was made with, between each
// provider's guard (its safety check before and after, ADR 0006). What it does is written to out.
func applyPlan(ctx context.Context, st *store.Store, site *sitedef.Site, digest string, o applyOptions, out io.Writer) (applyResult, error) {
	p, in, err := st.LoadPlan(ctx, digest)
	if errors.Is(err, store.ErrNotFound) {
		return applyResult{}, &noPlanError{digest}
	}
	if err != nil {
		return applyResult{}, told{fmt.Sprintf("MAM-REC-0001: %v\n  hint: the stored plan or its inputs changed; do not apply it, make a new plan", err)}
	}
	programs, err := st.PlanPrograms(ctx, p.Digest)
	if err != nil {
		return applyResult{}, err
	}
	nodes := &node.Provider{Store: st, Clock: wallClock}
	var rt *external.Runtime
	var prov reconcile.Provider = nodes
	if len(programs) > 0 {
		t, err := o.Tools()
		if err != nil {
			return applyResult{}, err
		}
		if rt, err = startPrograms(ctx, programs, t, o.ProvidersFile); err != nil {
			return applyResult{}, err
		}
		defer rt.Close()
		if err := setRecords(ctx, rt, st); err != nil {
			return applyResult{}, err
		}
		rt.SetStack(in.Snapshot.Stack)
		// What the other plans of the last hour are to create is taken (not this plan's own).
		claims, err := st.Claims(ctx, wallClock().Add(-claimWindow), p.Digest)
		if err != nil {
			return applyResult{}, err
		}
		rt.SetClaims(claims)
		made := &artifacts{Cache: o.ImageCache, Site: site, Nodes: st}
		if site != nil && site.ControlPlane.Enabled() {
			keys, err := site.SSHPublicKeys()
			if err != nil {
				return applyResult{}, fmt.Errorf("guest.ssh_public_key_file: %w", err)
			}
			if made.Seeds, err = seedMaker(site, st, keys); err != nil {
				return applyResult{}, err
			}
		} else if site != nil {
			// No Control Plane: machines are made without mamelet (the declaration has nothing for it to do).
			keys, err := site.SSHPublicKeys()
			if err != nil {
				return applyResult{}, fmt.Errorf("guest.ssh_public_key_file: %w", err)
			}
			var keyList []string
			if keys != "" {
				keyList = strings.Split(keys, "\n")
			}
			made.Seeds = &seed.Maker{User: site.Guest.User, SSHPublicKeys: keyList, Plain: true}
		}
		rt.SetArtifacts(made)
		rt.SetLimits(siteLimits(site))
	}
	providers := site.Providers().DigestWith(rt.Refs())
	if rt != nil {
		prov = &external.Provider{Base: nodes, Runtime: rt, Graph: in.Revision, Store: st, Clock: wallClock, Providers: providers}
	}

	// 1. Each provider's guard, with the changes of it this plan may carry out. (A plan made for
	// other providers is refused by the reconciler before anything is contacted: MAM-REC-0007.)
	var before []external.GuardReport
	if rt != nil && in.Providers == providers {
		before = rt.Guard(ctx, protocol.GuardBefore, plannedChanges(p), nil)
		if len(before) > 0 {
			fmt.Fprintln(out, "== guard (before)")
			printGuard(out, before)
			if blocked(before) {
				fmt.Fprintln(out, "\nguard: BLOCKED. Nothing was changed.")
				return applyResult{BlockedBefore: true}, nil
			}
			fmt.Fprintln(out)
		}
	}

	// 2. Reconcile.
	fmt.Fprintf(out, "== apply %s\n", p.Digest)
	host, _ := os.Hostname()
	rec := &reconcile.Reconciler{Store: st, Provider: prov, Owner: fmt.Sprintf("%s/%d", host, os.Getpid()), Lease: 3 * time.Hour, Providers: providers}
	rep, applyErr := rec.Apply(ctx, p.Digest)
	if rep != nil {
		printReport(out, rep)
	}

	// 3. The guards again, whatever happened above.
	var after []external.GuardReport
	if len(before) > 0 {
		baselines := map[string][]byte{}
		for _, g := range before {
			baselines[g.Provider] = g.Result.Baseline
		}
		after = rt.Guard(ctx, protocol.GuardAfter, nil, baselines)
		fmt.Fprintln(out, "\n== guard (after)")
		printGuard(out, after)
	}

	switch {
	case applyErr != nil:
		return applyResult{}, applyErr
	case blocked(after):
		fmt.Fprintln(out, "\nguard: BLOCKED after the apply. Something the provider guards changed; investigate before any further apply.")
		return applyResult{BlockedAfter: true, Stopped: rep.Stopped}, nil
	}
	return applyResult{Stopped: rep.Stopped}, nil
}

// mameletServedWarning says, when a declaration pins a mamelet this Control Plane does not serve,
// that mamelet on the node could not fetch it ("" when all is well or nothing is pinned). It only
// advises: the plan does not depend on it, and an update that cannot fetch its binary fails safely.
func mameletServedWarning(site *sitedef.Site, g *graph.Graph) string {
	var pinned []string
	for _, r := range g.Sorted() {
		if r.NodeMamelet != nil {
			pinned = append(pinned, r.NodeMamelet.SHA256)
		}
	}
	if len(pinned) == 0 || site.ControlPlane.MameletBinary == "" {
		return ""
	}
	bin, err := os.ReadFile(site.ControlPlane.MameletBinary)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(bin)
	served := hex.EncodeToString(sum[:])
	for _, sha := range pinned {
		if sha != served {
			return fmt.Sprintf("MAM-NODE-0103: the declaration pins mamelet %s, but this Control Plane serves %s (controlplane.mamelet_binary): a node could not fetch the pinned one; put its binary there and restart mamela serve, or pin %s", sha[:12], served[:12], served[:12])
		}
	}
	return ""
}
