package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/store"
)

// mameletTrust is what a mamelet pins about its Control Plane, as the members of its
// /etc/mamelet/config.json that hold it.
type mameletTrust struct {
	CA          string `json:"ca"`
	RevisionKey []byte `json:"revisionKey"`
}

func printTrust(w io.Writer, pki *cp.PKI) error {
	b, err := json.Marshal(mameletTrust{CA: string(pki.CAPEM()), RevisionKey: pki.RevisionPublicKey()}, json.Deterministic(true))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s\n", b)
	return nil
}

// runTrust prints the two members of a mamelet's config.json that name this Control Plane: the CA it
// is pinned to and the key that signs its revisions.
func runTrust(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mamela controlplane trust", flag.ContinueOnError)
	flags.SetOutput(stderr)
	db := flags.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	sitePath := addSiteFlag(flags)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: mamela controlplane trust [--site <site.toml>] [--db <state.db>]\n\nPrints the \"ca\" and \"revisionKey\" members of /etc/mamelet/config.json for this Control Plane (read-only).")
		return 2
	}
	site, err := loadSite(*sitePath)
	if err == nil && (site == nil || !site.ControlPlane.Enabled()) {
		err = fmt.Errorf("the site has no [controlplane] section (see mamela.site.example.toml)")
	}
	if err != nil {
		return report(stderr, err)
	}
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	if missing := cp.MissingKeys(dir); len(missing) > 0 {
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0201: %s lacks %s: there is no CA and revision key to print\n  hint: restore them (mamela restore --in <backup>)", dir, strings.Join(missing, ", "))})
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	pki, err := loadPKI(ctx, site, st)
	if err != nil {
		return report(stderr, err)
	}
	if err := printTrust(stdout, pki); err != nil {
		return report(stderr, err)
	}
	return 0
}

// runNewKeys makes a new CA, revision key and server certificate for a database that has mamelets
// registered. It is the one way to do that, and only where there are none: a Control Plane never
// makes them by itself for such a database (MAM-PKI-0201). Every registered mamelet is pinned to
// the old ones, so each must be enrolled again with the new (docs/recovery.md).
func runNewKeys(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mamela controlplane new-keys", flag.ContinueOnError)
	flags.SetOutput(stderr)
	db := flags.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	sitePath := addSiteFlag(flags)
	confirm := flags.Bool("confirm", false, "make the keys (without it: only say what would happen)")
	flags.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela controlplane new-keys [--confirm] [flags]\n\n"+
			"Makes a new CA, revision signing key and server certificate in controlplane.state_dir, keeping the database.\n"+
			"For when the keys are lost, or to replace them. No mamelet trusts the new CA: every registered node must be\n"+
			"enrolled again, and its config.json given the new CA and revision key (docs/recovery.md). The directory must\n"+
			"hold none of the CA and revision key files: move the old ones away yourself.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	site, err := loadSite(*sitePath)
	if err == nil && (site == nil || !site.ControlPlane.Enabled()) {
		err = fmt.Errorf("the site has no [controlplane] section (see mamela.site.example.toml)")
	}
	if err != nil {
		return report(stderr, err)
	}
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	if missing := cp.MissingKeys(dir); len(missing) != len(cp.Files[:3]) {
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0302: %s already holds some of the Control Plane's keys; nothing was changed\n"+
			"  hint: if they are the ones to keep, restore the rest (mamela restore --in <backup>). To replace them, move the CA and revision key files away first (they are the old identity: keep them until every node is enrolled again)", dir)})
	}
	if _, err := os.Stat(*db); err != nil {
		return report(stderr, told{fmt.Sprintf("there is no database at %s\n  hint: --db (or MAMELA_DB) names the Control Plane's state.db", *db)})
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	agents, err := st.Agents(ctx)
	if err != nil {
		return report(stderr, err)
	}
	if !*confirm {
		fmt.Fprintf(stdout, "Would make a new CA, revision key and server certificate in %s.\n"+
			"%d registered mamelet(s) would then trust nothing this Control Plane signs, until each is enrolled again:\n", dir, len(agents))
		for _, a := range agents {
			fmt.Fprintf(stdout, "  %s (VMID %d)\n", a.Node, a.VMID)
		}
		fmt.Fprintf(stdout, "Run again with --confirm to do it.\n")
		return 0
	}
	pki, err := cp.LoadPKI(dir, hostsOf(site.ControlPlane.URL), wallClock())
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "made a new CA, revision key and server certificate in %s.\n\n", dir)
	fmt.Fprintf(stdout, "Every registered mamelet is pinned to the old ones. For each node (%s):\n", nodeNames(agents))
	fmt.Fprintf(stdout, "  1. mamela controlplane reenroll --node <name>   (a one-time token)\n"+
		"  2. on the node: put these two members into /etc/mamelet/config.json (replacing \"ca\" and \"revisionKey\"):\n\n")
	if err := printTrust(stdout, pki); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "\n  3. on the node: write the token to enrollTokenFile, rm <stateDir>/cert.pem, systemctl restart mamelet\n"+
		"(mamela controlplane trust prints the two members again; docs/recovery.md has the whole procedure)\n")
	return 0
}

func nodeNames(agents []store.Agent) string {
	var names []string
	for _, a := range agents {
		names = append(names, a.Node)
	}
	return strings.Join(names, ", ")
}
