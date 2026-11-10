package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// runDestroy destroys one VM Mamela created, after a person has seen exactly what goes and typed
// its name. It is separate from plan and apply on purpose: removing a declaration never destroys
// anything, and plans never contain deletions.
func runDestroy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela destroy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	confirm := fs.String("confirm", "", "the VM's name again, to destroy it (without it, only show what would be destroyed)")
	baseline := fs.String("baseline", ".mamela/lab-baseline.json", "where to record the guests outside the lab before destroying")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela destroy [flags] <vm-name>\n\n"+
			"Destroys a VM that Mamela created in the lab (pool, VMID range, tag, ownership marker and Mamela's VMID\n"+
			"reservation must all agree), with its disks, after preflight; then forgets it and checks with postflight\n"+
			"that nothing outside the lab changed. Without --confirm it only shows what would be destroyed.\n"+
			"Uploaded images and seeds are left on the storage: `mamela lab cleanup` prints how to remove them.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	audit, err := labClient(site, site.Proxmox.AuditTokenID, site.Proxmox.AuditTokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	prov, err := labVMProvider(site, operating, st, "", false)
	if err != nil {
		return report(stderr, err)
	}
	target, err := prov.DestroyTarget(ctx, name)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "VM %s (VMID %d, %s, marker %s) would be destroyed with its disks; this cannot be undone:\n",
		target.Name, target.VMID, target.Status, target.State)
	for _, v := range target.Volumes {
		fmt.Fprintf(stdout, "  %s\n", v)
	}
	if len(target.Seeds) > 0 {
		fmt.Fprintf(stdout, "Its cloud-init seed stays on the storage (%s); `mamela lab cleanup` prints how to remove it.\n", strings.Join(target.Seeds, ", "))
	}
	fmt.Fprintln(stdout, "Mamela then forgets its VMID reservation and its mamelet's enrollment (the agent's certificate stops working).")
	writeUses(stdout, "", site.Uses(proxmox.NeedsFor(pve.DestroyCalls)))
	if *confirm != name {
		fmt.Fprintf(stdout, "\nNothing was changed. To destroy it: mamela destroy --site %s --confirm %s %s\n", *sitePath, name, name)
		return 1
	}

	fmt.Fprintln(stdout, "\n== preflight")
	facts, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, err)
	}
	findings := lab.CheckFor(site, facts, 0)
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

	fmt.Fprintf(stdout, "\n== destroy %s (VMID %d)\n", name, target.VMID)
	_ = st.AppendEvent(ctx, "", "VirtualMachine "+name, "destroy", fmt.Sprintf("destroying VMID %d (confirmed by name)", target.VMID))
	destroyErr := prov.Destroy(ctx, operating.WithDestroy(), target)
	if destroyErr == nil {
		if err := st.ForgetVM(ctx, name); err != nil {
			destroyErr = fmt.Errorf("VM destroyed, but forgetting it in the database failed: %w", err)
		} else {
			fmt.Fprintf(stdout, "destroyed %s (VMID %d) and forgot it\n", name, target.VMID)
			_ = st.AppendEvent(ctx, "", "VirtualMachine "+name, "destroyed", fmt.Sprintf("VMID %d destroyed and forgotten", target.VMID))
		}
	}

	fmt.Fprintln(stdout, "\n== postflight")
	after, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, fmt.Errorf("postflight: %w", err))
	}
	post := lab.Compare(site, before, after)
	printFindings(stdout, post)
	switch {
	case destroyErr != nil:
		return report(stderr, destroyErr)
	case lab.Blocked(post):
		fmt.Fprintln(stdout, "\npostflight: BLOCKED. Something outside the lab changed; investigate.")
		return 1
	}
	return 0
}

// runCleanup prints a script, for a person to review and run as root, that removes the images and
// seeds Mamela uploaded and no VM in the lab pool uses any more. Mamela's token cannot delete
// files: that would need Datastore.Allocate on the whole storage, which also holds other guests'
// backups and templates.
func runCleanup(ctx context.Context, site *lab.Site, stdout, stderr io.Writer) int {
	operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	used := map[string]bool{}
	res, err := operating.Resources(ctx)
	if err != nil {
		return report(stderr, err)
	}
	for _, r := range res {
		if r.Type != "qemu" || r.Node != site.Proxmox.Node {
			continue
		}
		cfg, err := operating.VMConfig(ctx, r.Node, r.VMID)
		if err != nil {
			return report(stderr, fmt.Errorf("read VM %d: %w", r.VMID, err))
		}
		for k := range cfg {
			v, _, _ := strings.Cut(cfg.String(k), ",")
			used[v] = true
			if _, src, ok := strings.Cut(cfg.String(k), "import-from="); ok {
				src, _, _ = strings.Cut(src, ",")
				used[src] = true
			}
		}
	}
	var free []string
	for _, content := range []string{"iso", "import"} {
		vols, err := operating.StorageContent(ctx, site.Proxmox.Node, site.Placement.ImageStorage, content)
		if err != nil {
			return report(stderr, err)
		}
		for _, v := range vols {
			_, file, _ := strings.Cut(v.VolID, "/")
			if strings.HasPrefix(file, "mamela-") && !used[v.VolID] {
				free = append(free, v.VolID)
			}
		}
	}
	slices.Sort(free)
	fmt.Fprintln(stdout, "#!/usr/bin/env bash")
	fmt.Fprintln(stdout, "# Mamela lab cleanup — generated by `mamela lab cleanup`. Review every line before running it as root.")
	fmt.Fprintln(stdout, "# It removes only files Mamela uploaded (names starting with mamela-) that no VM in the lab uses.")
	fmt.Fprintln(stdout, "set -euo pipefail")
	if len(free) == 0 {
		fmt.Fprintln(stdout, "echo 'nothing to remove'")
	}
	for _, v := range free {
		fmt.Fprintf(stdout, "pvesm free '%s'\n", v)
	}
	return 0
}
