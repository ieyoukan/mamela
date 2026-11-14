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

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/seed"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// runApply carries out an approved plan (applyPlan).
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
	res, err := applyPlan(ctx, st, site, fs.Arg(0), applyOptions{ProvidersFile: *providersFile, ImageCache: *imageCache, Tools: tf.find}, stdout)
	var noPlan *noPlanError
	switch {
	case errors.As(err, &noPlan):
		fmt.Fprintf(stderr, "%s in %s\n", noPlan, *db)
		return 1
	case err != nil:
		return report(stderr, err)
	case !res.OK():
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
		fmt.Fprintln(w, "no operations were executed in this phase")
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
	if rep.RemainingErr != "" {
		fmt.Fprintf(w, "\nMAM-REC-0102: could not observe after applying (%s); the outcomes above are recorded, but whether the declaration converged is not known. Make a new plan.\n", rep.RemainingErr)
	}
	if rep.Remaining != nil {
		fmt.Fprintf(w, "\nAfter apply: %s\n", plan.Summary(rep.Remaining))
		for _, c := range rep.Remaining.Changes {
			if c.Status != plan.StatusInSync && c.Status != plan.StatusRetained {
				fmt.Fprintln(w, "MAM-REC-0101: the declaration has not converged; make a new plan after the next observation. Any new changes need their own policy decision and approval.")
				break
			}
		}
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
