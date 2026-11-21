package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/ieyoukan/mamela/internal/pin"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

// pinClient is the HTTP client of mamela pin (nil: Go's default transport); tests point it at
// their servers.
var pinClient *http.Client

// pinner makes mamela pin's Pinner (tests replace it).
var pinner = func() *pin.Pinner { return &pin.Pinner{Client: pinClient} }

// runPin writes the sha256 of what a declaration downloads into its mamela.sum (docs/design/
// declaration-inputs.md, 2): it is the only command that downloads for a declaration.
func runPin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela pin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "only report what "+sumfile.Name+" lacks and what it no longer needs (no network, writes nothing; fails when it lacks something)")
	verify := fs.Bool("verify", false, "download every URL of "+sumfile.Name+" again and compare (writes nothing)")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela pin [--check | --verify] [flags] <pome-dir>\n\n"+
			"Evaluates the declaration, finds the downloads it declares without their sha256 (Mamela.artifact_pinned,\n"+
			"Mamela.apt_repo_pinned, K3s.server_pinned, K3s.agent_pinned), downloads each one "+sumfile.Name+" lacks\n"+
			"(https only, at most 512 MiB) and writes its sha256 there, without the lines no longer needed.\n"+
			"A GitHub release's asset is compared with the digest GitHub lists for it. Evaluating never\n"+
			"downloads: commit "+sumfile.Name+" with the declaration.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || (*check && *verify) {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	pins, err := t.evaluator().Pins(ctx, dir)
	if err != nil {
		return report(stderr, err)
	}
	mode := pin.Update
	switch {
	case *check:
		mode = pin.Check
	case *verify:
		mode = pin.Verify
	}
	if err := pinner().Run(ctx, dir, pins, mode, stdout); err != nil {
		return report(stderr, err)
	}
	return 0
}
