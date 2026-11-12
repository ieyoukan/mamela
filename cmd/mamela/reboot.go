package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/provider/pve"
)

// runReboot restarts one running VM Mamela created, after a person has seen which and typed its
// name: the way out for a VM stuck at boot (plan says so with MAM-PLAN-0110). It is imperative
// like destroy, not part of a plan, and it changes nothing but the VM's power state.
func runReboot(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela reboot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	confirm := fs.String("confirm", "", "the VM's name again, to reboot it (without it, only show what would be rebooted)")
	hard := fs.Bool("hard", false, "reset the VM at once instead of asking its guest to restart (for a guest that does not respond)")
	baseline := fs.String("baseline", ".mamela/lab-baseline.json", "where to record the guests outside the lab before rebooting")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela reboot [flags] <vm-name>\n\n"+
			"Restarts a running VM that Mamela created in the lab (pool, VMID range, tag, ownership marker and Mamela's\n"+
			"VMID reservation must all agree), after preflight, and checks with postflight that nothing outside the lab\n"+
			"changed. By default the guest is asked to restart (ACPI); --hard resets it at once, which a guest stuck at\n"+
			"boot needs. Without --confirm it only shows what would be rebooted.\n\nFlags:\n")
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
	how := "asked to restart (a clean shutdown, then a start)"
	if *hard {
		how = "reset at once (like its reset button: anything unsaved in the guest is lost)"
	}
	fmt.Fprintf(stdout, "VM %s (VMID %d, %s) would be %s.\n", target.Name, target.VMID, target.Status, how)
	renderProxmoxUses(stdout, site, "reboot uses", pve.RebootCalls)
	if target.Status != "running" {
		fmt.Fprintf(stdout, "\nIt is not running; nothing to reboot. `mamela plan` starts a stopped VM (start-vm).\n")
		return 1
	}
	if *confirm != name {
		flags := ""
		if *hard {
			flags = " --hard"
		}
		fmt.Fprintf(stdout, "\nNothing was changed. To reboot it: mamela reboot --site %s%s --confirm %s %s\n", *sitePath, flags, name, name)
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

	fmt.Fprintf(stdout, "\n== reboot %s (VMID %d)\n", name, target.VMID)
	_ = st.AppendEvent(ctx, "", "VirtualMachine "+name, "reboot", fmt.Sprintf("rebooting VMID %d (hard %v, confirmed by name)", target.VMID, *hard))
	rebootErr := prov.Reboot(ctx, operating, target, *hard)
	if rebootErr == nil {
		fmt.Fprintf(stdout, "rebooted %s (VMID %d)\n", name, target.VMID)
		_ = st.AppendEvent(ctx, "", "VirtualMachine "+name, "rebooted", fmt.Sprintf("VMID %d rebooted", target.VMID))
	}

	fmt.Fprintln(stdout, "\n== postflight")
	after, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, fmt.Errorf("postflight: %w", err))
	}
	post := lab.Compare(site, before, after)
	printFindings(stdout, post)
	switch {
	case rebootErr != nil:
		return report(stderr, rebootErr)
	case lab.Blocked(post):
		fmt.Fprintln(stdout, "\npostflight: BLOCKED. Something outside the lab changed; investigate.")
		return 1
	}
	return 0
}
