package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/ieyoukan/mamela/internal/scaffold"
)

// runProviderNew writes a new provider's repository (ADR 0009).
func runProviderNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "the provider's name (default: the directory's name without a leading mamela-)")
	module := fs.String("module", "", "the Pome's Emela module (default: from the name: foo-bar is FooBar)")
	api := fs.String("api", "", "the API's name, as manifests and catalogs give it (default: <name>/v1)")
	mamela := fs.String("mamela", "", "a local checkout of Mamela to build against (Go replace and the Pome's [replace]), "+
		"until the versions it needs are published")
	mamelaVersion := fs.String("mamela-version", "", "the version of github.com/ieyoukan/mamela to require (default: this mamela's)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	goModule, dir := fs.Arg(0), fs.Arg(1)
	if *name == "" {
		*name = strings.TrimPrefix(filepath.Base(dir), "mamela-")
	}
	v := *mamelaVersion
	switch {
	case v != "":
	case *mamela != "":
		v = "v0.0.0-00010101000000-000000000000" // replaced by the checkout
	default:
		// A build of a published version (go install ...@v...) names it; a development build does not.
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path == "github.com/ieyoukan/mamela" &&
			strings.HasPrefix(bi.Main.Version, "v") && !strings.Contains(bi.Main.Version, "+") {
			v = bi.Main.Version
		}
		if v == "" {
			return report(stderr, errors.New("this mamela is a development build, which names no published version of Mamela: "+
				"give --mamela <checkout> (or --mamela-version)"))
		}
	}
	written, err := scaffold.Write(dir, scaffold.Options{GoModule: goModule, Provider: *name, Module: *module, API: *api,
		MamelaVersion: v, Mamela: *mamela})
	if err != nil {
		return report(stderr, err)
	}
	for _, f := range written {
		fmt.Fprintf(stdout, "  %s\n", filepath.Join(dir, f))
	}
	fmt.Fprintf(stdout, `
provider %s: a kind of example ("thing") against a fake of its API. Next:

  cd %s
  go mod tidy
  ./build.sh        # the program, into the Pome, and the Pome's Emela (pome/src/lib.emel)
  go test ./...     # Mamela's conformance suite against the fake API

Then replace the thing with what your API manages: the calls, List, Diff and Done in the provider,
the catalog (what each call does), and the fake. See docs/provider-authoring.md of Mamela.
`, *name, dir)
	return 0
}
