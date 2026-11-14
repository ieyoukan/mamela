package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/internal/version"
)

const apiUsage = `Usage: mamela api --socket <path> [flags]

Serves the Control Plane's local API (ADR 0012) on a Unix socket, for mamelacd: plan, approve and
apply, as the CLI does (the same database, guards, policy and catalogs), and nothing else. The
socket is mode 0660; give it to mamelacd's group with --socket-group.

Flags:
`

func runAPI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "the Unix socket to serve on")
	group := fs.String("socket-group", "", "the group given the socket (mamelacd's)")
	workDir := fs.String("work-dir", "", "where trees are extracted (default: <db>.trees)")
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	providersFile := addProvidersFlag(fs)
	policyFile := fs.String("policy", "", "policy JSON file (default: the built-in conservative policy)")
	imageCache := fs.String("image-cache", defaultImageCache(), "directory of downloaded, verified files (images)")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, apiUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *socket == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *workDir == "" {
		*workDir = *db + ".trees"
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	b := &apiBackend{st: st, tools: t, sitePath: *sitePath, providers: *providersFile, policy: *policyFile, imageCache: *imageCache}
	logger := log.New(stderr, "mamela api: ", log.LstdFlags)
	srv := &api.Server{Backend: b, WorkDir: *workDir, Mamela: version.Current(), Logf: logger.Printf}
	l, err := api.Listen(*socket, *group)
	if err != nil {
		return report(stderr, err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	fmt.Fprintf(stdout, "serving the Control Plane's API (%s) on %s; Ctrl-C to stop\n", api.Version, *socket)
	if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return report(stderr, err)
	}
	return 0
}

// apiBackend answers the API with the same operations the CLI runs (service.go), against the
// Control Plane's open database. What it returns is read by mamelacd and posted on GitHub, so
// the Control Plane's local paths do not leave through it (hidden).
type apiBackend struct {
	st         *store.Store
	tools      tools
	sitePath   string
	providers  string
	policy     string
	imageCache string
}

func (b *apiBackend) findTools() (tools, error) { return b.tools, nil }

// hidden is s without the path of the declaration's directory in the extracted tree: evaluating
// it can name the path in the toolchain's own messages.
func hidden(s, dir string) string { return strings.ReplaceAll(s, dir, "<declaration>") }

func (b *apiBackend) Plan(ctx context.Context, dir string) (api.Planned, error) {
	res, err := planDeclaration(ctx, b.st, dir, planOptions{SitePath: b.sitePath, ProvidersFile: b.providers,
		PolicyFile: b.policy, Tools: b.findTools})
	if err != nil {
		return api.Planned{}, errors.New(hidden(rendered(err), dir))
	}
	p := res.Plan
	out := api.Planned{Digest: p.Digest, Revision: p.Inputs.RevisionDigest}
	for _, c := range p.Changes {
		switch {
		case c.Status == plan.StatusBlocked || c.Verdict == policy.Denied:
			out.Blocked++
		case c.Status == plan.StatusWaiting:
			out.Waiting++
		case c.Status == plan.StatusChange:
			out.Changes++
		}
	}
	for _, pr := range res.Programs {
		out.Programs = append(out.Programs, fmt.Sprintf("%s %s (%s, %s)", pr.Provider, pr.Version, pr.Source, pr.Digest))
	}
	var text bytes.Buffer
	plan.Render(&text, p)
	out.Text = hidden(text.String(), dir)
	return out, nil
}

func (b *apiBackend) Revision(ctx context.Context, dir string) (string, error) {
	res, err := b.tools.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return "", errors.New(hidden(err.Error(), dir))
	}
	return res.Graph.Digest(), nil
}

func (b *apiBackend) Approve(ctx context.Context, digest, by string, ttl time.Duration) error {
	if _, err := approvePlan(ctx, b.st, digest, by, ttl); err != nil {
		return errors.New(rendered(err))
	}
	return nil
}

func (b *apiBackend) Approved(ctx context.Context, digest string) (bool, error) {
	a, err := b.st.ValidApproval(ctx, digest)
	if err != nil {
		return false, err
	}
	return a != nil, nil
}

// Apply reports a failed apply in Applied (OK false, its text), not as an error: the API itself
// worked.
func (b *apiBackend) Apply(ctx context.Context, digest string) (api.Applied, error) {
	var text bytes.Buffer
	site, err := loadSite(b.sitePath)
	if err == nil {
		var res applyResult
		res, err = applyPlan(ctx, b.st, site, digest, applyOptions{ProvidersFile: b.providers, ImageCache: b.imageCache, Tools: b.findTools}, &text)
		if err == nil {
			return api.Applied{OK: res.OK(), Text: text.String(), Stale: res.Stale()}, nil
		}
	}
	report(&text, err)
	return api.Applied{Text: text.String()}, nil
}
