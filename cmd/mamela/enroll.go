package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// runEnroll writes the script that installs mamelet on a machine that exists already (one Mamela did
// not make: an adopted container or VM) and enrolls it, with a one-time token for that machine. The
// Control Plane never logs into the machine: a person reads the script and runs it there as root.
// Only a machine Mamela manages is given one: a plan has seen it as Mamela's (made or adopted), so
// the Control Plane has published a revision for it, with the VMID it observed.
func runEnroll(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela controlplane enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	sitePath := addSiteFlag(fs)
	policyFile := fs.String("policy", "", "policy JSON file in force, as for mamela plan: mamelet installs packages only from its allowedAptOrigins")
	node := fs.String("node", "", "the machine's name in the declaration (required)")
	vmid := fs.Int("vmid", 0, "the machine's ID at its provider (a Proxmox VMID), as the plan observed it (required)")
	out := fs.String("out", "", "the script to write (required; mode 0600, never over a file that exists)")
	hostname := fs.String("hostname", "", "the machine's hostname (uname -n), when it is not the node's name: the script runs only on it")
	ttl := fs.Duration("ttl", time.Hour, "how long the token stays good (at most 24h)")
	again := fs.Bool("reenroll", false, "also for a node that has a mamelet registered: enrolling replaces the registration")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela controlplane enroll --node <name> --vmid <id> --out <file> [--hostname <h>] [--ttl 1h] [flags]\n\n"+
			"Writes a script that installs mamelet on a machine that exists already (one Mamela did not make, adopted)\n"+
			"and enrolls it, with a one-time token for that node and VMID. Read the script, then run it as root on the\n"+
			"machine; the Control Plane never logs into it. Only for a machine Mamela manages (made or adopted, and planned).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2 // Parse has printed the usage
	}
	if fs.NArg() != 0 || *node == "" || *vmid == 0 || *out == "" {
		fs.Usage()
		return 2
	}
	if *ttl <= 0 || *ttl > maxReenrollTTL {
		return report(stderr, fmt.Errorf("--ttl must be more than 0 and at most %s: the token is a password for the node's identity", maxReenrollTTL))
	}
	if !noderev.ValidNode(*node) || *vmid < 0 {
		return report(stderr, fmt.Errorf("--node %q --vmid %d: not a node's name and ID", *node, *vmid))
	}
	if *hostname == "" {
		*hostname = *node
	}
	site, err := loadSite(*sitePath)
	if err == nil && (site == nil || !site.ControlPlane.Enabled()) {
		err = fmt.Errorf("the site has no [controlplane] section (see mamela.site.example.toml): there is no Control Plane for mamelet to enroll with")
	}
	if err != nil {
		return report(stderr, err)
	}
	if _, err := os.Lstat(*out); err == nil {
		return report(stderr, told{fmt.Sprintf("%s exists; nothing was issued\n  hint: name another file with --out (the script is never written over a file)", *out)})
	}
	if _, err := os.Stat(*db); err != nil {
		return report(stderr, told{fmt.Sprintf("there is no database at %s: %v\n  hint: --db (or MAMELA_DB) names the Control Plane's state.db (/var/lib/mamela/state.db on its host)", *db, err)})
	}
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	// The keys of the Control Plane that runs: an enrollment never makes them (the running one would not know them).
	if missing := cp.MissingKeys(dir); len(missing) > 0 {
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0201: %s lacks %s: there is no CA for mamelet to trust; nothing was issued\n"+
			"  hint: run this on the Control Plane's host with its site (--site /etc/mamela/site.toml), after mamela serve has run once; restore lost keys with mamela restore", dir, strings.Join(missing, ", "))})
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()

	// A machine Mamela manages has a revision: a plan saw it as Mamela's and published it, with its ID.
	rev, err := st.NodeRevision(ctx, *node)
	if err != nil {
		return report(stderr, err)
	}
	switch {
	case rev == nil:
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0303: node %s is not a machine this Control Plane manages: no plan has seen it as Mamela's (made by Mamela, or adopted), so there is no revision for its mamelet; nothing was issued\n"+
			"  hint: adopt it first: declare it with its ID, mamela plan shows it to adopt (MAM-PLAN-0114), run its provider's adopt operation (mamela act --confirm %s <adopt-op> <resource> <pome-dir>), and plan again; then run this (docs/design/adopting-existing-machines.md)", *node, *node)})
	case rev.VMID != *vmid:
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0304: node %s is VMID %d, as the plan observed it, not %d; nothing was issued: a token for another machine would give it this node's revisions\n"+
			"  hint: check the machine's ID at its provider; if the node is now another machine, plan again first", *node, rev.VMID, *vmid)})
	}
	if a, registered, err := st.Agent(ctx, *node); err != nil {
		return report(stderr, err)
	} else if registered && !*again {
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0305: node %s has a mamelet registered already (VMID %d, enrolled %s); nothing was issued\n"+
			"  hint: for a mamelet whose certificate ended: mamela controlplane reenroll --node %s (docs/recovery.md). To install mamelet on it anew, replacing the registration: add --reenroll, and run the script with --reenroll",
			*node, a.VMID, a.Enrolled.Format("2006-01-02"), *node)})
	}
	pol, err := currentPolicy(site, *policyFile)
	if err != nil {
		return report(stderr, err)
	}
	if len(pol.AllowedAptOrigins) == 0 {
		return report(stderr, told{"the policy allows no APT origin (allowedAptOrigins), and mamelet needs at least one; nothing was issued\n  hint: name the origins packages may come from in the policy (--policy)"})
	}
	maker, err := seedMaker(site, st, "")
	if err != nil {
		return report(stderr, err)
	}
	maker.AllowedOrigins = pol.AllowedAptOrigins

	// The file first (never over another), then the token: a token is not issued for a script that is not written.
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return report(stderr, err)
	}
	now := wallClock()
	script, err := maker.NewEnrollScript(ctx, *node, *vmid, *hostname, site.ControlPlane.Advertised(), *ttl, now)
	if err == nil {
		_, err = f.Write(script)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(*out)
		return report(stderr, errors.Join(errors.New("the script was not written"), err))
	}

	name := filepath.Base(*out)
	fmt.Fprintf(stdout, "wrote %s (mode 0600): it installs mamelet on %s (VMID %d) and enrolls it, with a one-time token for\n"+
		"that node and VMID, good for %s (until %s). The token is its only secret: the CA certificate and\n"+
		"the revision key in it are public.\n\n", *out, *node, *vmid, *ttl, now.Add(*ttl).UTC().Format("2006-01-02 15:04 MST"))
	reach := site.ControlPlane.URL
	if adv := site.ControlPlane.Advertised(); adv != reach {
		reach += " and " + adv
	}
	fmt.Fprintf(stdout, "The machine must reach the Control Plane at %s: it downloads mamelet from there (by its sha256)\n"+
		"and enrolls there. The script runs only on a machine whose hostname is %s.\n\n", reach, *hostname)
	fmt.Fprintf(stdout, "Read the script, then run it as root on the machine. How it gets there is up to you; for example:\n"+
		"  a Proxmox LXC container: copy %s to its Proxmox host, then there\n"+
		"    pct push %d %s /root/mamelet-enroll.sh --perms 0600\n"+
		"    pct exec %d -- sh /root/mamelet-enroll.sh\n"+
		"    pct exec %d -- rm /root/mamelet-enroll.sh\n"+
		"  a VM, over SSH as a user that may become root:\n"+
		"    scp %s <user>@<address>:mamelet-enroll.sh\n"+
		"    ssh -t <user>@<address> 'sudo sh mamelet-enroll.sh && rm mamelet-enroll.sh'\n\n",
		*out, *vmid, name, *vmid, *vmid, *out)
	fmt.Fprintf(stdout, "Watch it enroll:\n"+
		"  the script waits up to a minute for it and says \"enrolled as %s (VMID %d)\"; on the machine: journalctl -u mamelet -f\n"+
		"  here: mamela controlplane agents --db %s   (lists %s)\n"+
		"  then: mamela plan <pome-dir>   (shows what mamelet observes on %s)\n\n", *node, *vmid, *db, *node, *node)
	if d, held := pol.Held(*node); held {
		fmt.Fprintf(stdout, "Node %s is held (%s: %s).\n  Its mamelet observes and reports, and is given no permission to change anything.\n", *node, d.Code, d.Message)
	}
	fmt.Fprintf(stdout, "mamelet installs packages only from the APT origins %s (the policy's allowedAptOrigins).\n", strings.Join(pol.AllowedAptOrigins, ", "))
	debian := false
	for _, o := range pol.AllowedAptOrigins {
		debian = debian || strings.HasPrefix(o, "Debian")
	}
	if !debian {
		fmt.Fprintf(stdout, "  For a Debian machine, add \"Debian:\" and \"Debian-Security:\" to the policy, and write the script again with --policy.\n")
	}
	fmt.Fprintf(stdout, "Delete %s, here and on the machine, once the node has enrolled.\n", *out)
	return 0
}
