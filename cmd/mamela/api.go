package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/internal/version"
)

const apiUsage = `Usage: mamela api --socket <path> [flags]

Serves the Control Plane's local API (ADR 0012) on a Unix socket, for mamelacd: plan, approve and
apply, as the CLI does (the same database, guards, policy and catalogs), and nothing else. The
socket is mode 0660; give it to mamelacd's group with --socket-group. Told to stop (SIGTERM), it takes
no new request, lets a running apply finish for up to --drain-timeout, and then cancels it.

Flags:
`

func runAPI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "the Unix socket to serve on")
	group := fs.String("socket-group", "", "the group given the socket (mamelacd's)")
	workDir := fs.String("work-dir", "", "where trees are extracted (default: <db>.trees)")
	drain := fs.Duration("drain-timeout", defaultDrain, "when stopped (SIGTERM), how long to let a running request (an apply) finish before it is stopped; set the service's TimeoutStopSec above it")
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
	if *socket == "" || fs.NArg() != 0 || *drain <= 0 {
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
	// The requests' contexts do not end when the program is told to stop: what runs is let finish (see
	// serveAPI). abort ends them when it has been given long enough.
	requests, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	hs := srv.HTTPServer(logger)
	hs.BaseContext = func(net.Listener) context.Context { return requests }
	fmt.Fprintf(stdout, "serving the Control Plane's API (%s) on %s; Ctrl-C to stop\n", api.Version, *socket)
	giveUp := func(ctx context.Context) {
		// Whatever this process claimed and did not finish: its outcome is unknown, and the next apply
		// observes before it does anything (not left for a lease of hours).
		if n, err := st.ReleaseOwner(ctx, reconcile.WorkerName(), "mamela api was stopped before it finished"); err != nil {
			logger.Printf("releasing the operations this process had not finished: %v", err)
		} else if n > 0 {
			logger.Printf("released %d operation(s) this process had not finished; their outcome is unknown, and the next apply observes first", n)
		}
	}
	if err := serveAPI(ctx, hs, l, *drain, srv.Stop, abort, giveUp, logger.Printf); err != nil {
		return report(stderr, err)
	}
	return 0
}

// defaultDrain is how long a stopped mamela api lets a running request finish. An apply can take as
// long as an image import, which no restart should wait for; the unit's TimeoutStopSec is a minute
// more than this.
const defaultDrain = 10 * time.Minute

// serveAPI serves hs on l until ctx is done, and then stops as a service should. It stops taking new
// requests (stop makes those already waiting their turn refuse), lets the running ones finish, an
// apply included, for up to drain, and returns only when none runs. A request still running after
// drain is cancelled (abort) and given a little time to end; giveUp then records what it left
// unfinished as of unknown outcome, and the program ends.
func serveAPI(ctx context.Context, hs *http.Server, l net.Listener, drain time.Duration, stop func(), abort context.CancelFunc,
	giveUp func(context.Context), logf func(format string, args ...any)) error {
	served := make(chan error, 1)
	go func() { served <- hs.Serve(l) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	stop()
	logf("stopping: not taking new requests; letting the ones that are running finish (up to %s)", drain)
	dctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := hs.Shutdown(dctx); err != nil {
		logf("%s passed and a request is still running: stopping it (its operation's outcome becomes unknown, and the next apply observes first)", drain)
		abort()
		gctx, cancelGrace := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelGrace()
		if err := hs.Shutdown(gctx); err != nil {
			hs.Close()
		}
		giveUp(context.Background())
	}
	<-served
	return nil
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
		if c.Deletes() {
			out.Removals++
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
	if _, err := approvePlan(ctx, b.st, digest, by, ttl, false); err != nil {
		if errors.Is(err, api.ErrNeedsHost) {
			return fmt.Errorf("%w; approve it on the Control Plane's host: mamela approve %s", err, digest)
		}
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
		res, err = applyPlan(ctx, b.st, site, digest, applyOptions{ProvidersFile: b.providers, PolicyFile: b.policy, ImageCache: b.imageCache, Tools: b.findTools}, &text)
		if err == nil {
			return api.Applied{OK: res.OK(), Text: text.String(), Stale: res.Stale()}, nil
		}
	}
	report(&text, err)
	return api.Applied{Text: text.String()}, nil
}
