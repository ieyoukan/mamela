package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/store"
)

// runApply carries out an approved plan on the lab: preflight, reconcile, postflight.
func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	imageCache := fs.String("image-cache", defaultImageCache(), "directory of downloaded, verified cloud images")
	baseline := fs.String("baseline", ".mamela/lab-baseline.json", "where to record the guests outside the lab before applying")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela apply [flags] <plan-digest>\n\n"+
			"Carries out an approved plan on the lab Proxmox (docs/lab.md). It runs the read-only preflight first and\n"+
			"stops if anything blocks, observes again and changes only what the plan expected, then checks with the\n"+
			"postflight that nothing outside the lab changed. Denied operations never run.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	p, _, err := st.LoadPlan(ctx, fs.Arg(0))
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(stderr, "no plan %s in %s\n", fs.Arg(0), *db)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "MAM-REC-0001: %v\n  hint: the stored plan or its inputs changed; do not apply it, make a new plan\n", err)
		return 1
	}

	audit, err := labClient(site, site.Proxmox.AuditTokenID, site.Proxmox.AuditTokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}

	// 1. Preflight for the VMs this plan may create.
	fmt.Fprintln(stdout, "== preflight")
	facts, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, err)
	}
	findings := lab.CheckFor(site, facts, creates(p))
	printFindings(stdout, findings)
	if lab.Blocked(findings) {
		fmt.Fprintln(stdout, "\npreflight: BLOCKED. Nothing was changed.")
		return 1
	}
	before := lab.NewBaseline(site, facts)
	if err := os.MkdirAll(filepath.Dir(*baseline), 0o700); err != nil {
		return report(stderr, err)
	}
	if err := os.WriteFile(*baseline, before.Encode(), 0o600); err != nil {
		return report(stderr, err)
	}

	// 2. Reconcile.
	fmt.Fprintf(stdout, "\n== apply %s\n", p.Digest)
	prov, err := labProvider(site, operating, st, *imageCache)
	if err != nil {
		return report(stderr, err)
	}
	host, _ := os.Hostname()
	rec := &reconcile.Reconciler{Store: st, Provider: prov, Owner: fmt.Sprintf("%s/%d", host, os.Getpid()), Lease: 3 * time.Hour}
	rep, applyErr := rec.Apply(ctx, p.Digest)
	if rep != nil {
		printReport(stdout, rep)
	}

	// 3. Postflight, whatever happened above.
	fmt.Fprintln(stdout, "\n== postflight")
	after, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, fmt.Errorf("postflight: %w", err))
	}
	post := lab.Compare(site, before, after)
	printFindings(stdout, post)

	switch {
	case applyErr != nil:
		return report(stderr, applyErr)
	case lab.Blocked(post):
		fmt.Fprintln(stdout, "\npostflight: BLOCKED. Something outside the lab changed; investigate before any further live test.")
		return 1
	case rep.Stopped != nil:
		return 1
	}
	return 0
}

// creates counts the VMs the plan would create.
func creates(p *plan.Plan) int {
	n := 0
	for _, c := range p.Changes {
		if c.Status != plan.StatusChange {
			continue
		}
		for _, op := range c.Operations {
			if op.Op == policy.OpCreateVM {
				n++
			}
		}
	}
	return n
}

func printFindings(w io.Writer, fs []lab.Finding) {
	for _, f := range fs {
		fmt.Fprintf(w, "%-5s %s  %s\n", f.Severity, f.Code, f.Message)
	}
}

func printReport(w io.Writer, rep *reconcile.Report) {
	if len(rep.Steps) == 0 && rep.Stopped == nil {
		fmt.Fprintln(w, "nothing to do: the lab already matches the plan")
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

// labProvider is the Proxmox provider for the lab, using the operating token.
func labProvider(site *lab.Site, api pve.API, st *store.Store, imageCache string) (*pve.Provider, error) {
	keys, err := site.SSHPublicKeys()
	if err != nil {
		return nil, fmt.Errorf("guest.ssh_public_key_file: %w", err)
	}
	return &pve.Provider{
		Config: pve.Config{
			Node: site.Proxmox.Node, Pool: site.Boundary.Pool, Tag: site.Boundary.Tag,
			VMIDMin: site.Boundary.VMIDMin, VMIDMax: site.Boundary.VMIDMax,
			DiskStorage: site.Placement.DiskStorage, ImageStorage: site.Placement.ImageStorage, Bridge: site.Placement.Bridge,
			MaxVMs: site.Budget.MaxVMs, MaxCPU: int64(site.Budget.MaxVCPUPerVM),
			MaxMemoryMiB: site.Budget.MaxMemoryMiBPerVM, MaxDiskGiB: site.Budget.MaxDiskGiBPerVM,
			User: site.Guest.User, SSHPublicKeys: keys,
		},
		API: api, VMIDs: st, Images: &pve.ImageCache{Dir: imageCache}, Clock: wallClock,
	}, nil
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
