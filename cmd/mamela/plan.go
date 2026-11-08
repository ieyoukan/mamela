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

	"github.com/ieyoukan/mamela/internal/evaluate"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

const defaultDB = ".mamela/state.db"

// wallClock is the only place the CLI reads the current time; planning itself never does.
var wallClock = func() time.Time { return time.Now().UTC() }

func openStore(ctx context.Context, path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return store.Open(ctx, path, wallClock)
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	snapshotFile := fs.String("snapshot", "", "observed state as a snapshot JSON file (default: an empty world, observed now)")
	replay := fs.String("replay", "", "recompute a stored plan by digest and check that it is unchanged")
	toolchainDir := fs.String("toolchain", envOr("MAMELA_TOOLCHAIN_DIR", ".tools/bin"), "directory containing the pinned emela and wasmtime")
	pomeCache := fs.String("pome-cache", defaultPomeCache(), "directory of already fetched Pome dependencies")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela plan [flags] <pome-dir>\n       mamela plan --replay <plan-digest>\n\n"+
			"Shows what would change, why, and what needs approval. Saves the plan so it can be approved.\n"+
			"Nothing outside the state database is changed.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()

	if *replay != "" {
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		p, _, err := st.LoadPlan(ctx, *replay)
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "no plan %s in %s\n", *replay, *db)
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "MAM-REC-0001: %v\n  hint: the stored plan or its inputs changed; do not apply it, make a new plan\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "plan %s recomputes to the same digest from its stored inputs\n\n", p.Digest)
		plan.Render(stdout, p)
		return 0
	}

	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	tc, err := evaluate.ToolchainInDir(*toolchainDir)
	if err != nil {
		return report(stderr, err)
	}
	e := &evaluate.Evaluator{Toolchain: tc, Limits: evaluate.DefaultLimits, PomeCache: *pomeCache}
	res, err := e.Evaluate(ctx, fs.Arg(0))
	if err != nil {
		return report(stderr, err)
	}

	snap := state.Empty(wallClock())
	if *snapshotFile != "" {
		b, err := os.ReadFile(*snapshotFile)
		if err != nil {
			return report(stderr, err)
		}
		if snap, err = state.Decode(b); err != nil {
			return report(stderr, err)
		}
	}
	pol := policy.Default
	p := plan.Build(res.Graph, snap, pol)
	if err := st.SavePlan(ctx, res.Graph, snap, pol, p); err != nil {
		return report(stderr, err)
	}
	plan.Render(stdout, p)
	fmt.Fprintf(stdout, "\nSaved to %s. To approve: mamela approve %s\n", *db, p.Digest)
	return 0
}

func runApprove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	by := fs.String("by", envOr("USER", ""), "who approves")
	ttl := fs.Duration("ttl", time.Hour, "how long the approval stays valid")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela approve [flags] <plan-digest>\n\n"+
			"Approves exactly this plan. Any change to the declaration, the observation or the policy makes a\n"+
			"different plan, which needs its own approval. Denied operations never run, approved or not.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *by == "" || *ttl <= 0 {
		fs.Usage()
		return 2
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	p, _, err := st.LoadPlan(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "cannot approve %s: %v\n", fs.Arg(0), err)
		return 1
	}
	a, err := st.Approve(ctx, p.Digest, *by, *ttl)
	if err != nil {
		return report(stderr, err)
	}
	denied := 0
	for _, c := range p.Changes {
		if c.Verdict == policy.Denied {
			denied++
		}
	}
	fmt.Fprintf(stdout, "approved %s by %s until %s\n", p.Digest, a.Approver, a.ExpiresAt.Format(time.RFC3339))
	if denied > 0 {
		fmt.Fprintf(stdout, "note: %d change%s in this plan %s denied and will not run\n", denied, plural(denied), choose(denied == 1, "is", "are"))
	}
	return 0
}
