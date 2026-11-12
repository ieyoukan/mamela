package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/pomegen"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/host"
)

// runProviderPome generates a provider Pome's Emela (src/lib.emel) from its program's manifest
// (ADR 0009): the kinds' spec fields become record types and declaring functions.
func runProviderPome(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider pome", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "src/lib.emel", "the file to write, in the Pome's directory")
	check := fs.Bool("check", false, "write nothing; fail if the file is not what would be generated (for CI)")
	tf := addToolFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	dir := fs.Arg(0)
	pkg, err := external.LoadPackage(dir)
	if err != nil {
		return report(stderr, err)
	}
	var pome struct {
		Pome struct {
			Module string `toml:"module"`
		} `toml:"pome"`
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "Pome.toml"), &pome); err != nil {
		return report(stderr, fmt.Errorf("%s: %w", filepath.Join(dir, "Pome.toml"), err))
	}
	exe, err := host.InspectBytes(dir, pkg.Program)
	if err != nil {
		return report(stderr, err)
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	s, err := host.Start(ctx, exe, host.Options{Wasmtime: t.toolchain.Wasmtime})
	if err != nil {
		return report(stderr, err)
	}
	m := s.Manifest()
	s.Close()
	src, err := pomegen.Generate(m, pomegen.Options{Module: pome.Pome.Module})
	if err != nil {
		return report(stderr, fmt.Errorf("provider %s: %w", m.Provider, err))
	}
	// Formatted as emela fmt formats it, so that the file is what an author would keep.
	formatted, err := emelaFmt(ctx, t.toolchain.Emela, src)
	if err != nil {
		return report(stderr, err)
	}
	path := filepath.Join(dir, *out)
	if *check {
		old, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(old, formatted) {
			fmt.Fprintf(stderr, "mamela: %s is not what the provider's manifest generates: run `mamela provider pome %s`\n", path, dir)
			return 1
		}
		fmt.Fprintf(stdout, "%s is up to date\n", path)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return report(stderr, err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return report(stderr, err)
	}
	n := 0
	for _, k := range m.Kinds {
		if len(k.Spec) > 0 {
			n++
		}
	}
	fmt.Fprintf(stdout, "wrote %s: %d kinds of provider %s %s\n", path, n, m.Provider, m.Version)
	return 0
}
