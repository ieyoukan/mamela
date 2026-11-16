package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ieyoukan/mamela/internal/provider/external"
)

// runProviderPin shows what mamela.providers.toml pins for the providers of a Pome or a
// declaration (ADR 0015): the digests an operator puts in it, after reading what they pin, and
// whether the file says so now. It writes nothing.
func runProviderPin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider pin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providersFile := addProvidersFlag(fs)
	check := fs.Bool("check", false, "exit 1 unless every pin is in the file and is the package's")
	tf := addToolFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	dir := fs.Arg(0)
	cfg, err := external.LoadConfig(*providersFile)
	if err != nil {
		return report(stderr, err)
	}
	var pkgs []external.Package
	if pkg, err := external.LoadPackage(dir); err == nil {
		pkgs = []external.Package{pkg} // a provider's Pome
	} else {
		t, err := tf.find()
		if err != nil {
			return report(stderr, err)
		}
		if pkgs, err = external.Discover(dir, t.pomeCache); err != nil { // a declaration's providers
			return report(stderr, err)
		}
	}
	found, pinned := false, true
	for _, pkg := range pkgs {
		if fs.NArg() == 2 && pkg.Provider != fs.Arg(1) {
			continue
		}
		found = true
		fmt.Fprintf(stdout, "# provider %s %s", pkg.Provider, pkg.ProviderVersion)
		if pkg.Source != "" {
			fmt.Fprintf(stdout, " (%s@%s)", pkg.Source, pkg.Version)
		}
		fmt.Fprintln(stdout)
		if _, ok := cfg.Provider[pkg.Provider]; !ok {
			fmt.Fprintf(stdout, "# %s has no [provider.%s] yet: these go with its api_url and token\n", *providersFile, pkg.Provider)
		}
		last := ""
		for _, l := range cfg.PinLines(pkg) {
			if l.Section != last {
				fmt.Fprintf(stdout, "%s\n", l.Section)
				last = l.Section
			}
			fmt.Fprintf(stdout, "%s = %q  # %s: %s\n", l.Key, l.Want, l.What, l.State())
			pinned = pinned && l.State() == "pinned"
		}
		fmt.Fprintf(stdout, "# read what they pin first: mamela provider catalog %s %s; the program is what `mamela provider inspect` shows\n\n", dir, pkg.Provider)
	}
	if !found {
		if fs.NArg() == 2 {
			return report(stderr, fmt.Errorf("no provider %s among the Pomes of %s", fs.Arg(1), dir))
		}
		return report(stderr, fmt.Errorf("%s is neither a provider's Pome nor a declaration with providers", dir))
	}
	if !pinned {
		fmt.Fprintf(stdout, "Put the lines above in the sections of %s, keeping the rest. With strict_pins = true at its top, a provider that is not pinned does not run (ADR 0015).\n", *providersFile)
		if *check {
			return 1
		}
	}
	return 0
}
