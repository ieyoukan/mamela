package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/plan"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const defaultDB = ".mamela/state.db"

// claimWindow is how long a plan's creates are taken for other plans: as long as it can be
// approved and its approval used (an approval lasts an hour from when it is given).
const claimWindow = 2 * time.Hour

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
	sitePath := addSiteFlag(fs)
	replay := fs.String("replay", "", "recompute a stored plan by digest and check that it is unchanged")
	tf := addToolFlags(fs)
	policyFile := fs.String("policy", "", "policy JSON file (default: the built-in conservative policy)")
	providersFile := addProvidersFlag(fs)
	jsonOutput := fs.Bool("json", false, "write the stored plan as JSON")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela plan [flags] <pome-dir>\n       mamela plan --replay <plan-digest>\n\n"+
			"Shows what would change, why, and what needs approval. Saves the plan so it can be approved.\n"+
			"Nothing outside the state database is changed. With a site that has a Control Plane, it also updates\n"+
			"what each node's mamelet observes (never what it may change: that takes an approved apply).\n\nFlags:\n")
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
	res, err := planDeclaration(ctx, st, fs.Arg(0), planOptions{SitePath: *sitePath, ProvidersFile: *providersFile,
		PolicyFile: *policyFile, SnapshotFile: *snapshotFile, Tools: tf.find})
	if err != nil {
		return report(stderr, err)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, w)
	}
	p := res.Plan
	if *jsonOutput {
		data, err := json.Marshal(planJSON{Digest: p.Digest, Plan: p}, json.Deterministic(true))
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	plan.Render(stdout, p)
	if res.Site != nil {
		renderPlanUses(stdout, res.Site, p, res.Graph, res.Policy, res.Manifests)
	}
	fmt.Fprintf(stdout, "\nSaved to %s. To approve: mamela approve --db %s %s\n", *db, shellQuote(*db), p.Digest)
	return 0
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type planJSON struct {
	Digest string     `json:"digest"`
	Plan   *plan.Plan `json:"plan"`
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
	res, err := approvePlan(ctx, st, fs.Arg(0), *by, *ttl)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "approved %s by %s until %s\n", res.Approval.PlanDigest, res.Approval.Approver, res.Approval.ExpiresAt.Format(time.RFC3339))
	if n := res.Denied; n > 0 {
		fmt.Fprintf(stdout, "note: %d change%s in this plan %s denied and will not run\n", n, plural(n), choose(n == 1, "is", "are"))
	}
	return 0
}

// siteRefusals says which machines the plan would create with a network the site does not allow:
// providers' machines whose seed names a network (ADR 0004).
func siteRefusals(site *sitedef.Site, p *plan.Plan) []string {
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
	}
	return out
}
