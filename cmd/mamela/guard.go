package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// runGuard runs the safety check of each provider of the declaration that has one (ADR 0006), as
// apply does before it changes anything, without changes: read-only.
func runGuard(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providersFile := addProvidersFlag(fs)
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela guard [flags] <pome-dir>\n\n"+
			"Runs the safety check of each provider of the declaration (its guard), as apply does before it\n"+
			"changes anything. The guards only read, with their own credentials (guard_token_id). Exit status 1\n"+
			"when anything blocks.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	rt, err := startProviders(ctx, fs.Arg(0), t, *providersFile)
	if err != nil {
		return report(stderr, err)
	}
	if rt == nil {
		fmt.Fprintln(stdout, "the declaration uses no provider")
		return 0
	}
	defer rt.Close()
	reports := rt.Guard(ctx, protocol.GuardBefore, nil, nil)
	if len(reports) == 0 {
		fmt.Fprintln(stdout, "no provider of the declaration has a guard")
		return 0
	}
	printGuard(stdout, reports)
	if blocked(reports) {
		fmt.Fprintln(stdout, "\nguard: BLOCKED. An apply would change nothing.")
		return 1
	}
	return 0
}
