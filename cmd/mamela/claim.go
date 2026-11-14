package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
)

// runClaim gives a declaration the objects it declares and Mamela made before it recorded which
// declaration made them (ADR 0013): from then on, removing them from it deletes them. Only a person
// runs it, on the host, and only with --confirm.
func runClaim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	providersFile := addProvidersFlag(fs)
	confirm := fs.Bool("confirm", false, "record it")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela claim [flags] <pome-dir>\n\n"+
			"Lists the objects the declaration declares that Mamela made before it recorded which declaration made\n"+
			"them, and, with --confirm, records that this declaration (its Pome's name) did. Removing one of them\n"+
			"from the declaration then plans its deletion, which needs an approval on this host (ADR 0013).\n"+
			"Without --confirm, nothing is changed.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	res, err := t.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return report(stderr, err)
	}
	if res.Stack == "" {
		return report(stderr, errors.New(`the Pome has no name: give it one in Pome.toml ([pome] name = "...") to claim objects for it`))
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	rt, err := startProviders(ctx, dir, t, *providersFile)
	if err != nil {
		return report(stderr, err)
	}
	if rt == nil {
		return report(stderr, errors.New("the declaration uses no provider"))
	}
	defer rt.Close()
	if err := setRecords(ctx, rt, st); err != nil {
		return report(stderr, err)
	}
	rt.SetStack(res.Stack)
	provided, err := rt.Observe(ctx, res.Graph, wallClock())
	if err != nil {
		return report(stderr, err)
	}
	claimed := 0
	for _, id := range slices.Sorted(maps.Keys(provided)) {
		p := provided[id]
		if !p.Declared || !p.Observed.Exists || !p.Observed.Owned {
			continue
		}
		stack, err := st.CreationStack(ctx, p.Provider, id)
		if err != nil {
			return report(stderr, err)
		}
		if stack != "" {
			continue
		}
		claimed++
		fmt.Fprintf(stdout, "%s  via %s\n", id, p.Provider)
		if *confirm {
			if err := st.ClaimCreations(ctx, res.Stack, p.Provider, id); err != nil {
				return report(stderr, err)
			}
		}
	}
	switch {
	case claimed == 0:
		fmt.Fprintf(stdout, "nothing to claim: every object %s declares and Mamela made is recorded for a declaration already\n", res.Stack)
	case *confirm:
		fmt.Fprintf(stdout, "\nrecorded %d object%s for %s\n", claimed, plural(claimed), res.Stack)
	default:
		fmt.Fprintf(stdout, "\nNothing was changed. To record %d object%s for %s: mamela claim --confirm %s\n", claimed, plural(claimed), res.Stack, dir)
	}
	return 0
}
