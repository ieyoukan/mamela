package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ieyoukan/mamela/internal/store"
)

func runRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	by := fs.String("by", envOr("USER", ""), "who revokes")
	reason := fs.String("reason", "", "why (kept with the revocation)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela revoke [flags] <plan-digest>\n\n"+
			"Ends the approval of a plan before it expires: apply no longer runs what needed it, and an apply\n"+
			"that is under way stops before the operations it has not begun (what it already did stays done).\n"+
			"The approval and its revocation stay in the database as records; the plan can be approved again.\n"+
			"The digest may be a prefix of 8 or more hex digits.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *by == "" {
		fs.Usage()
		return 2
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	digest, err := st.ResolvePlanDigest(ctx, fs.Arg(0))
	if err != nil {
		return report(stderr, err)
	}
	a, err := st.Revoke(ctx, digest, *by, *reason)
	switch {
	case errors.Is(err, store.ErrNotApproved):
		fmt.Fprintf(stderr, "MAM-APR-0001: plan %s has no valid approval to revoke; nothing was changed\n"+
			"  hint: it was never approved, its approval expired, or it was revoked already (mamela apply shows what runs)\n", digest)
		return 1
	case err != nil:
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "revoked the approval of %s (by %s, valid until %s); it runs nothing now\n", a.PlanDigest, a.Approver, a.ExpiresAt.Format(time.RFC3339))
	return 0
}
