package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/pomegen"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/tfimport"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const importUsage = `Usage: mamela import terraform [flags] <pome-dir>

Moves what Terraform or OpenTofu manages to Mamela (ADR 0009). Give it the JSON of the state
(terraform show -json > state.json; tofu show -json): Mamela never runs Terraform and never reads a
state file. The declaration's providers (the Pomes in <pome-dir>'s dependencies) turn each resource
of a type they import into a declaration, which it writes as Emela (src/imported.emel); it records
each object as one to adopt. Nothing is changed: plan shows each as to adopt (MAM-PLAN-0114), and a
person adopts each with its provider's adopt operation (mamela act --confirm). Then remove it from
Terraform's state (terraform state rm), so that only Mamela manages it.

Flags:
`

// runImport imports the resources of another tool (Terraform) as declarations to adopt.
func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "terraform" {
		fmt.Fprint(stderr, importUsage)
		return 2
	}
	fs := flag.NewFlagSet("mamela import terraform", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	statePath := fs.String("state", "-", "the JSON of `terraform show -json` (- for standard input)")
	out := fs.String("out", "src/imported.emel", "the Emela file to write, in <pome-dir>")
	force := fs.Bool("force", false, "replace the file if it exists")
	dryRun := fs.Bool("dry-run", false, "show what would be imported; write and record nothing")
	by := fs.String("by", envOr("USER", ""), "who imports")
	providersFile := addProvidersFlag(fs)
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, importUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	var data []byte
	var err error
	if *statePath == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*statePath)
	}
	if err != nil {
		return report(stderr, err)
	}
	objects, err := tfimport.Read(data)
	if err != nil {
		return report(stderr, fmt.Errorf("MAM-IMP-0001: %w\n  hint: give the output of `terraform show -json` (or `tofu show -json`) in the Terraform directory", err))
	}
	path := filepath.Join(dir, *out)
	if _, err := os.Stat(path); err == nil && !*force && !*dryRun {
		return report(stderr, fmt.Errorf("MAM-IMP-0002: %s exists; nothing was imported\n  hint: --force replaces it, or --out names another file", path))
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	pkgs, err := external.Discover(dir, t.pomeCache)
	if err != nil {
		return report(stderr, err)
	}
	if len(pkgs) == 0 {
		return report(stderr, errors.New("MAM-IMP-0003: the declaration uses no provider\n  hint: add the providers' Pomes to its Pome.toml (emela pome add)"))
	}
	rt, err := startPrograms(ctx, external.Programs(pkgs), t, "")
	if err != nil {
		return report(stderr, err)
	}
	defer rt.Close()
	// The guard's settings say what the site's declarations need that Terraform does not (an AWS
	// boundary); no API is contacted.
	cfg, err := external.LoadConfig(*providersFile)
	if err != nil {
		return report(stderr, err)
	}

	// Which provider imports each type: one, or the import is ambiguous.
	importer := map[string]string{}
	for _, p := range pkgs {
		s, _ := rt.Session(p.Provider)
		for _, k := range s.Manifest().Kinds {
			for _, typ := range k.Imports {
				if other, dup := importer[typ]; dup && other != p.Provider {
					return report(stderr, fmt.Errorf("MAM-IMP-0004: providers %s and %s both import %s", other, p.Provider, typ))
				}
				importer[typ] = p.Provider
			}
		}
	}
	byProvider := map[string][]protocol.ImportObject{}
	var unknown []string
	for _, o := range objects {
		if p, ok := importer[o.Type]; ok {
			byProvider[p] = append(byProvider[p], o)
		} else {
			unknown = append(unknown, o.Address)
		}
	}
	var all []pomegen.Imports
	type adoption struct {
		provider, resource, address string
		ids                         map[string]string
		op                          protocol.Op
		also                        []string
	}
	var adoptions []adoption
	var skipped []protocol.ImportSkip
	for _, p := range pkgs {
		objs := byProvider[p.Provider]
		if len(objs) == 0 {
			continue
		}
		s, _ := rt.Session(p.Provider)
		settings, _, err := cfg.ScriptInput(p.Provider)
		if err != nil {
			return report(stderr, err)
		}
		res, err := s.Import(ctx, objs, settings)
		if err != nil {
			return report(stderr, fmt.Errorf("provider %s: import: %w", p.Provider, err))
		}
		module, err := generatedModule(p.Dir)
		if err != nil {
			return report(stderr, fmt.Errorf("provider %s: %w", p.Provider, err))
		}
		all = append(all, pomegen.Imports{Manifest: s.Manifest(), Module: module, Resources: res.Resources})
		skipped = append(skipped, res.Skipped...)
		for _, r := range res.Resources {
			var op protocol.Op
			for _, k := range s.Manifest().Kinds {
				for _, o := range k.Operations {
					if k.Kind == r.Kind && o.Adopts && o.Support == protocol.Supported {
						op = o.Op
					}
				}
			}
			adoptions = append(adoptions, adoption{p.Provider, string(r.Kind) + " " + r.Name, r.Address, r.IDs, op, r.Also})
		}
	}
	n := len(adoptions)
	for _, a := range adoptions {
		n += len(a.also)
	}
	if len(adoptions) == 0 {
		fmt.Fprintf(stdout, "Nothing to import: no provider of the declaration imports these %d resources.\n", len(objects))
		printNotImported(stdout, skipped, unknown)
		return 1
	}
	src, err := pomegen.Declarations(all, pomegen.DeclareOptions{Source: "a Terraform state"})
	if err != nil {
		return report(stderr, fmt.Errorf("MAM-IMP-0005: %w", err))
	}
	formatted, err := emelaFmt(ctx, t.toolchain.Emela, src)
	if err != nil {
		return report(stderr, err)
	}
	slices.SortFunc(adoptions, func(a, b adoption) int { return strings.Compare(a.address, b.address) })
	fmt.Fprintf(stdout, "%d of %d resources import, as %d declarations:\n", n, len(objects), len(adoptions))
	for _, a := range adoptions {
		fmt.Fprintf(stdout, "  %-40s %s (provider %s)\n", a.address, a.resource, a.provider)
		for _, x := range a.also {
			fmt.Fprintf(stdout, "    with %s\n", x)
		}
	}
	printNotImported(stdout, skipped, unknown)
	if *dryRun {
		fmt.Fprintf(stdout, "\n--dry-run: nothing was written or recorded. %s would be:\n\n%s", path, formatted)
		return 0
	}
	if *by == "" {
		return report(stderr, errors.New("--by: who imports is required"))
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return report(stderr, err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return report(stderr, err)
	}
	for _, a := range adoptions {
		if err := st.RecordAdoption(ctx, a.provider, a.resource, a.ids, "terraform:"+a.address, *by); err != nil {
			return report(stderr, err)
		}
	}
	fmt.Fprintf(stdout, "\nwrote %s and recorded %d resources to adopt. Next:\n\n", path, len(adoptions))
	fmt.Fprintf(stdout, "  1. In the declaration: import Imported, and add |> Imported.imported to its graph.\n")
	fmt.Fprintf(stdout, "  2. mamela plan %s       each shows as to adopt (MAM-PLAN-0114); nothing changes them\n", dir)
	fmt.Fprintf(stdout, "  3. Adopt each (it becomes Mamela's; read what it sends first, without --confirm):\n")
	for _, a := range adoptions {
		name := strings.SplitN(a.resource, " ", 2)[1]
		if a.op == "" {
			fmt.Fprintf(stdout, "       %s: provider %s has no adopt operation for it; it stays to adopt\n", a.resource, a.provider)
			continue
		}
		fmt.Fprintf(stdout, "       mamela act --confirm %s %s %q %s\n", name, a.op, a.resource, dir)
	}
	fmt.Fprintf(stdout, "  4. Then Terraform must forget them, so that only Mamela manages them:\n")
	var addrs []string
	for _, a := range adoptions {
		addrs = append(addrs, "'"+a.address+"'")
		for _, x := range a.also {
			addrs = append(addrs, "'"+x+"'")
		}
	}
	fmt.Fprintf(stdout, "       terraform state rm %s\n", strings.Join(addrs, " "))
	return 0
}

func printNotImported(w io.Writer, skipped []protocol.ImportSkip, unknown []string) {
	if len(skipped)+len(unknown) == 0 {
		return
	}
	fmt.Fprintf(w, "not imported (Terraform keeps managing them):\n")
	slices.SortFunc(skipped, func(a, b protocol.ImportSkip) int { return strings.Compare(a.Address, b.Address) })
	for _, s := range skipped {
		fmt.Fprintf(w, "  %-40s %s\n", s.Address, s.Why)
	}
	types := map[string][]string{}
	for _, u := range unknown {
		types[u] = nil
	}
	for _, u := range slices.Sorted(maps.Keys(types)) {
		fmt.Fprintf(w, "  %-40s no provider of the declaration imports its type\n", u)
	}
}

// generatedModule is the module of a provider's Pome that mamela provider pome generated: its
// root module (src/lib.emel), or, when the root is written by hand, Kinds (src/kinds.emel).
func generatedModule(dir string) (string, error) {
	var p struct {
		Pome struct {
			Module string `toml:"module"`
		} `toml:"pome"`
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "Pome.toml"), &p); err != nil {
		return "", err
	}
	if p.Pome.Module == "" {
		return "", fmt.Errorf("%s: the Pome names no module", filepath.Join(dir, "Pome.toml"))
	}
	for _, f := range []struct{ file, module string }{{"src/lib.emel", p.Pome.Module}, {"src/kinds.emel", p.Pome.Module + ".Kinds"}} {
		src, err := os.ReadFile(filepath.Join(dir, f.file))
		if err == nil && bytes.Contains(src, []byte(pomegen.GeneratedMark)) {
			return f.module, nil
		}
	}
	return "", errors.New("its Pome has no generated module (src/lib.emel or src/kinds.emel by mamela provider pome) to declare what is imported with")
}

// emelaFmt formats Emela source as emela fmt does.
func emelaFmt(ctx context.Context, emela string, src []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, emela, "fmt", "-")
	cmd.Stdin = bytes.NewReader(src)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("emela fmt of the generated source: %v\n%s", err, errb.String())
	}
	return out.Bytes(), nil
}
