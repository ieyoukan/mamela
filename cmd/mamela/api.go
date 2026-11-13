package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
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

Serves the Control Plane's local API (ADR 0012) on a Unix socket, for mamelacd: plan a declaration
of a commit's tree, approve a plan by its digest, and apply an approved plan, exactly as the CLI
does (the same database, guards, policy and catalogs). Nothing else: no explicit operation, no
import. The socket is mode 0660; give it to mamelacd's group with --socket-group.

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
	tools := []string{"--toolchain", t.dir, "--pome-cache", t.pomeCache}
	b := &cliBackend{st: st, tools: t, db: *db, common: append([]string{"--db", *db, "--site", *sitePath, "--providers", *providersFile}, tools...),
		policy: *policyFile, imageCache: *imageCache}
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

// cliBackend does what the API is asked exactly as the CLI does it (the same commands, in process).
type cliBackend struct {
	st         *store.Store
	tools      tools
	db         string
	common     []string
	policy     string
	imageCache string
}

func (b *cliBackend) run(ctx context.Context, args ...string) (int, string) {
	var out bytes.Buffer
	code := run(ctx, args, &out, &out)
	return code, out.String()
}

func (b *cliBackend) Plan(ctx context.Context, dir string) (api.Planned, error) {
	args := append([]string{"plan", "--json"}, b.common...)
	if b.policy != "" {
		args = append(args, "--policy", b.policy)
	}
	var stdout, stderr bytes.Buffer
	code := run(ctx, append(args, dir), &stdout, &stderr)
	out := stdout.String()
	if code != 0 {
		return api.Planned{}, errors.New(strings.ReplaceAll(strings.TrimSpace(out+stderr.String()), dir, "<declaration>"))
	}
	var output planJSON
	if err := json.Unmarshal([]byte(out), &output, json.RejectUnknownMembers(true)); err != nil {
		return api.Planned{}, err
	}
	p, _, err := b.st.LoadPlan(ctx, output.Digest)
	if err != nil {
		return api.Planned{}, err
	}
	changes, waiting, blocked := 0, 0, 0
	for _, c := range p.Changes {
		switch {
		case c.Status == plan.StatusBlocked || c.Verdict == policy.Denied:
			blocked++
		case c.Status == plan.StatusWaiting:
			waiting++
		case c.Status == plan.StatusChange:
			changes++
		}
	}
	progs, err := b.st.PlanPrograms(ctx, p.Digest)
	if err != nil {
		return api.Planned{}, err
	}
	var names []string
	for _, pr := range progs {
		names = append(names, fmt.Sprintf("%s %s (%s, %s)", pr.Provider, pr.Version, pr.Source, pr.Digest))
	}
	// The text without the local paths of the Control Plane.
	var rendered bytes.Buffer
	plan.Render(&rendered, p)
	text := strings.ReplaceAll(rendered.String(), dir, "<declaration>")
	return api.Planned{Digest: p.Digest, Revision: p.Inputs.RevisionDigest, Changes: changes, Waiting: waiting, Blocked: blocked, Text: text, Programs: names}, nil
}

func (b *cliBackend) Revision(ctx context.Context, dir string) (string, error) {
	res, err := b.tools.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return "", errors.New(strings.ReplaceAll(err.Error(), dir, "<declaration>"))
	}
	return res.Graph.Digest(), nil
}

func (b *cliBackend) Approve(ctx context.Context, digest, by string, ttl time.Duration) error {
	code, out := b.run(ctx, "approve", "--db", b.db, "--by", by, "--ttl", ttl.String(), digest)
	if code != 0 {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

func (b *cliBackend) Approved(ctx context.Context, digest string) (bool, error) {
	a, err := b.st.ValidApproval(ctx, digest)
	if err != nil {
		return false, err
	}
	return a != nil, nil
}

func (b *cliBackend) Apply(ctx context.Context, digest string) (api.Applied, error) {
	args := append([]string{"apply", "--image-cache", b.imageCache}, b.common...)
	code, out := b.run(ctx, append(args, digest)...)
	// The world was not what the plan assumed: nothing was done for it (reason codes are stable).
	stale := code != 0 && strings.Contains(out, "stopped: MAM-REC-0002")
	return api.Applied{OK: code == 0, Text: out, Stale: stale}, nil
}
