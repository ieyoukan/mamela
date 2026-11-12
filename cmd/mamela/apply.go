package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/seed"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// runApply carries out an approved plan through the providers the plan was made with, between each
// provider's guard (its safety check before and after, ADR 0006).
func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	imageCache := fs.String("image-cache", defaultImageCache(), "directory of downloaded, verified files (images)")
	providersFile := addProvidersFlag(fs)
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela apply [flags] <plan-digest>\n\n"+
			"Carries out an approved plan. Resources of providers go through the providers the plan was made with\n"+
			"(the same programs, stored with the plan), each change under a permit for exactly its approved requests.\n"+
			"Each provider's guard checks its infrastructure first: if anything blocks, nothing is sent. After, the\n"+
			"guards check again (for example, that nothing outside the provider's boundary changed).\n"+
			"The world is observed again first, and only what the plan expected is changed. Denied operations never run.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	site, err := loadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	p, in, err := st.LoadPlan(ctx, fs.Arg(0))
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(stderr, "no plan %s in %s\n", fs.Arg(0), *db)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "MAM-REC-0001: %v\n  hint: the stored plan or its inputs changed; do not apply it, make a new plan\n", err)
		return 1
	}
	programs, err := st.PlanPrograms(ctx, p.Digest)
	if err != nil {
		return report(stderr, err)
	}
	nodes := &node.Provider{Store: st, Clock: wallClock}
	var rt *external.Runtime
	var prov reconcile.Provider = nodes
	if len(programs) > 0 {
		t, err := tf.find()
		if err != nil {
			return report(stderr, err)
		}
		if rt, err = startPrograms(ctx, programs, t, *providersFile); err != nil {
			return report(stderr, err)
		}
		defer rt.Close()
		if err := setRecords(ctx, rt, st); err != nil {
			return report(stderr, err)
		}
		// What the other plans of the last hour are to create is taken (not this plan's own).
		claims, err := st.Claims(ctx, wallClock().Add(-claimWindow), p.Digest)
		if err != nil {
			return report(stderr, err)
		}
		rt.SetClaims(claims)
		made := &artifacts{Cache: *imageCache, Site: site, Nodes: st}
		if site != nil && site.ControlPlane.Enabled() {
			keys, err := site.SSHPublicKeys()
			if err != nil {
				return report(stderr, fmt.Errorf("guest.ssh_public_key_file: %w", err))
			}
			if made.Seeds, err = seedMaker(site, st, keys); err != nil {
				return report(stderr, err)
			}
		}
		rt.SetArtifacts(made)
		rt.SetLimits(siteLimits(site))
	}
	digest := site.Providers().DigestWith(rt.Refs())
	if rt != nil {
		prov = &external.Provider{Base: nodes, Runtime: rt, Graph: in.Revision, Store: st, Clock: wallClock, Providers: digest}
	}

	// 1. Each provider's guard, with the changes of it this plan may carry out. (A plan made for
	// other providers is refused by the reconciler before anything is contacted: MAM-REC-0007.)
	var before []external.GuardReport
	if rt != nil && in.Providers == digest {
		before = rt.Guard(ctx, protocol.GuardBefore, plannedChanges(p), nil)
		if len(before) > 0 {
			fmt.Fprintln(stdout, "== guard (before)")
			printGuard(stdout, before)
			if blocked(before) {
				fmt.Fprintln(stdout, "\nguard: BLOCKED. Nothing was changed.")
				return 1
			}
			fmt.Fprintln(stdout)
		}
	}

	// 2. Reconcile.
	fmt.Fprintf(stdout, "== apply %s\n", p.Digest)
	host, _ := os.Hostname()
	rec := &reconcile.Reconciler{Store: st, Provider: prov, Owner: fmt.Sprintf("%s/%d", host, os.Getpid()), Lease: 3 * time.Hour, Providers: digest}
	rep, applyErr := rec.Apply(ctx, p.Digest)
	if rep != nil {
		printReport(stdout, rep)
	}

	// 3. The guards again, whatever happened above.
	var after []external.GuardReport
	if len(before) > 0 {
		baselines := map[string][]byte{}
		for _, g := range before {
			baselines[g.Provider] = g.Result.Baseline
		}
		after = rt.Guard(ctx, protocol.GuardAfter, nil, baselines)
		fmt.Fprintln(stdout, "\n== guard (after)")
		printGuard(stdout, after)
	}

	switch {
	case applyErr != nil:
		return report(stderr, applyErr)
	case blocked(after):
		fmt.Fprintln(stdout, "\nguard: BLOCKED after the apply. Something the provider guards changed; investigate before any further apply.")
		return 1
	case rep.Stopped != nil:
		return 1
	}
	return 0
}

// plannedChanges are, by provider, the changes of the plan that may run (denied ones never do).
func plannedChanges(p *plan.Plan) map[string][]protocol.Change {
	out := map[string][]protocol.Change{}
	for _, c := range p.Changes {
		if c.Status != plan.StatusChange || c.Provider == "" {
			continue
		}
		for i, pc := range c.ProviderChanges {
			if i < len(c.Operations) && c.Operations[i].Decision.Verdict == policy.Denied {
				continue
			}
			out[c.Provider] = append(out[c.Provider], pc)
		}
	}
	return out
}

func blocked(rs []external.GuardReport) bool {
	return slices.ContainsFunc(rs, external.GuardReport.Blocked)
}

func printGuard(w io.Writer, rs []external.GuardReport) {
	for _, g := range rs {
		if g.Err != nil {
			fmt.Fprintf(w, "%-5s %s  the guard failed: %v\n", "BLOCK", g.Provider, g.Err)
			continue
		}
		for _, f := range g.Result.Findings {
			sev := f.Severity
			if sev == protocol.SeverityBlock {
				sev = "BLOCK"
			}
			fmt.Fprintf(w, "%-5s %s %s  %s\n", sev, g.Provider, f.Code, f.Message)
		}
	}
}

func printReport(w io.Writer, rep *reconcile.Report) {
	if len(rep.Steps) == 0 && rep.Stopped == nil {
		fmt.Fprintln(w, "nothing to do: the world already matches the plan")
	}
	for _, s := range rep.Steps {
		line := fmt.Sprintf("%-14s %-16s %s", s.Outcome, s.Op, s.Resource)
		if s.Detail != "" {
			line += "  (" + s.Detail + ")"
		}
		fmt.Fprintln(w, line)
	}
	if s := rep.Stopped; s != nil {
		fmt.Fprintf(w, "\nstopped: %s: %s\n  hint: %s\n", s.Code, s.Message, s.Hint)
	}
}

// seedMaker makes cloud-init seeds that install mamelet and point it at this Control Plane.
func seedMaker(site *sitedef.Site, st *store.Store, keys string) (*seed.Maker, error) {
	pki, err := loadPKI(site)
	if err != nil {
		return nil, err
	}
	bin, err := os.ReadFile(site.ControlPlane.MameletBinary)
	if err != nil {
		return nil, fmt.Errorf("controlplane.mamelet_binary: %w (build it with scripts/build-mamelet.sh)", err)
	}
	// An ELF64 little-endian x86-64 executable: the machines are amd64.
	if len(bin) < 20 || string(bin[:4]) != "\x7fELF" || bin[4] != 2 || bin[5] != 1 || bin[18] != 0x3e {
		return nil, fmt.Errorf("%s is not a linux/amd64 executable (build it with scripts/build-mamelet.sh)", site.ControlPlane.MameletBinary)
	}
	var keyList []string
	if keys != "" {
		keyList = strings.Split(keys, "\n")
	}
	return &seed.Maker{Store: st, Server: site.ControlPlane.URL, CA: pki.CAPEM(), RevisionKey: pki.RevisionPublicKey(),
		User: site.Guest.User, SSHPublicKeys: keyList, AllowedOrigins: policy.Default.AllowedAptOrigins, MameletBinary: bin}, nil
}

// loadPKI loads (creating on first use) the Control Plane's keys; the server certificate names the
// host mamelet connects to.
func loadPKI(site *sitedef.Site) (*cp.PKI, error) {
	dir, err := sitedef.ExpandHome(site.ControlPlane.StateDir)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(site.ControlPlane.URL)
	if err != nil {
		return nil, err
	}
	return cp.LoadPKI(dir, []string{u.Hostname()}, wallClock())
}

func defaultImageCache() string {
	if v := os.Getenv("MAMELA_IMAGE_CACHE"); v != "" {
		return v
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "mamela", "images")
	}
	return ".mamela/images"
}
