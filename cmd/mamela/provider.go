package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const providerUsage = `Usage:
  mamela provider inspect [flags] <program>   check a provider program and show what it declares
  mamela provider trust --source <s> --version <v> <program>
                                              trust one native provider build (it runs outside any sandbox)
  mamela provider script [--providers <file>] <pome-dir> <provider> [<script>]
                                              print a setup script the provider writes (list them without <script>)

A provider is untrusted code. A WASM provider (GOOS=wasip1, or a WASI 0.3 component) runs isolated
in the pinned wasmtime: no files, environment or network; its API requests go through Mamela, which
checks each against the approved operation. A native provider runs only when trusted.
See docs/provider-authoring.md.
`

func runProvider(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	switch args[0] {
	case "inspect":
		return runProviderInspect(ctx, args[1:], stdout, stderr)
	case "trust":
		return runProviderTrust(args[1:], stdout, stderr)
	case "script":
		return runProviderScript(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, providerUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "mamela provider: unknown command %q\n\n%s", args[0], providerUsage)
		return 2
	}
}

func runProviderInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trustFile := fs.String("trust-file", "mamela.trust.toml", "the record of trusted native providers")
	source := fs.String("source", "", "the provider's source, for a native provider (as trusted)")
	ver := fs.String("version", "", "the provider's version, for a native provider (as trusted)")
	catalogPath := fs.String("catalog", "", "a catalog of the provider's API (JSON) to classify its calls with")
	asJSON := fs.Bool("json", false, "print the manifest and the classification as JSON")
	tf := addToolFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	exe, err := host.Inspect(fs.Arg(0))
	if err != nil {
		return report(stderr, err)
	}
	var catalog protocol.Catalog
	if *catalogPath != "" {
		data, err := os.ReadFile(*catalogPath)
		if err != nil {
			return report(stderr, err)
		}
		if err := json.Unmarshal(data, &catalog, json.RejectUnknownMembers(true)); err != nil {
			return report(stderr, fmt.Errorf("%s: %w", *catalogPath, err))
		}
	}
	trust, err := host.LoadTrust(*trustFile)
	if err != nil {
		return report(stderr, err)
	}
	o := host.Options{Source: *source, Version: *ver, Trust: trust, Catalog: catalog}
	if exe.Format != host.FormatNative {
		t, err := tf.find()
		if err != nil {
			return report(stderr, err)
		}
		o.Wasmtime = t.toolchain.Wasmtime
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "%s\n  format: %s\n  digest: %s\n", exe.Path, exe.Format, exe.Digest)
		if exe.Format == host.FormatNative {
			fmt.Fprintf(stdout, "  native: runs outside any sandbox, only when trusted (mamela provider trust)\n")
		} else {
			fmt.Fprintf(stdout, "  imports: %d, all allowed (standard input, output and error only; no files, environment or network)\n", len(exe.Imports))
		}
	}
	s, err := host.Start(ctx, exe, o)
	if err != nil {
		return report(stderr, err)
	}
	defer s.Close()
	m := s.Manifest()
	type opView struct {
		Kind      protocol.ResourceKind `json:"kind"`
		Op        protocol.Op           `json:"op"`
		Verified  protocol.Verified     `json:"verified"`
		Automatic bool                  `json:"autoApprovable"`
	}
	var ops []opView
	for _, k := range m.Kinds {
		for _, op := range k.Operations {
			if op.Support == protocol.Supported {
				v := s.Verify(op)
				ops = append(ops, opView{k.Kind, op.Op, v, v.AutoApprovable([]protocol.Effect{protocol.EffectRead, protocol.EffectCreate, protocol.EffectChange})})
			}
		}
	}
	if *asJSON {
		data, err := json.Marshal(struct {
			Format   host.Format       `json:"format"`
			Digest   string            `json:"digest"`
			Manifest protocol.Manifest `json:"manifest"`
			Ops      []opView          `json:"operations"`
		}{exe.Format, exe.Digest, m, ops}, json.Deterministic(true))
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "%s\n", data)
		return 0
	}
	fmt.Fprintf(stdout, "\n%s %s (api %s)\n", m.Provider, m.Version, orNone(m.API))
	fmt.Fprintf(stdout, "  observe: %s\n", calls(m.ObserveCalls))
	for _, k := range m.Kinds {
		for _, op := range k.Operations {
			if op.Support != protocol.Supported {
				fmt.Fprintf(stdout, "  %s %s: not supported (%s)\n", k.Kind, op.Op, op.Why)
				continue
			}
			v := s.Verify(op)
			effects := "none classified"
			if len(v.Effects) > 0 {
				var es []string
				for _, e := range v.Effects {
					es = append(es, string(e))
				}
				effects = strings.Join(es, ", ")
			}
			fmt.Fprintf(stdout, "  %s %s: %s\n    effects (from the catalog, not the provider): %s\n", k.Kind, op.Op, calls(op.Calls), effects)
			switch {
			case v.MayLoseData():
				fmt.Fprintf(stdout, "    may lose data: denied by default\n")
			case len(v.Unclassified) > 0:
				fmt.Fprintf(stdout, "    unclassified: %s; every such change needs a person's approval\n", calls(v.Unclassified))
			}
		}
	}
	return 0
}

func calls(cs []protocol.Call) string {
	if len(cs) == 0 {
		return "no calls"
	}
	var out []string
	for _, c := range cs {
		s := c.Method + " " + c.Path
		var ps []string
		if len(c.Query) > 0 {
			ps = append(ps, "query: "+strings.Join(c.Query, ", "))
		}
		if len(c.Form) > 0 {
			ps = append(ps, "form: "+strings.Join(c.Form, ", "))
		}
		if len(ps) > 0 {
			s += " (" + strings.Join(ps, "; ") + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func runProviderTrust(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider trust", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trustFile := fs.String("trust-file", "mamela.trust.toml", "the record of trusted native providers (review and commit it)")
	source := fs.String("source", "", "where the provider comes from (a Pome path, a repository) (required)")
	ver := fs.String("version", "", "the provider's version (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *source == "" || *ver == "" {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	exe, err := host.Inspect(fs.Arg(0))
	if err != nil {
		return report(stderr, err)
	}
	if exe.Format != host.FormatNative {
		fmt.Fprintf(stderr, "mamela: %s is a %s provider: it runs isolated and needs no trust\n", exe.Path, exe.Format)
		return 1
	}
	f, err := host.LoadTrust(*trustFile)
	if err != nil {
		return report(stderr, err)
	}
	f.Add(host.Trust{Source: *source, Version: *ver, Digest: exe.Digest})
	if err := f.Save(*trustFile); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "%s\n\ntrusted %s %s, %s\nrecorded in %s; review it like code\n", host.TrustWarning, *source, *ver, exe.Digest, *trustFile)
	return 0
}

// runProviderScript prints one of the scripts a provider of the declaration writes (ADR 0006): for
// a person to review and run with privileges Mamela never holds. The provider is the one the
// declaration's Pome.lock pins; it runs isolated, sends no request, and sees no secret.
func runProviderScript(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela provider script", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providersFile := addProvidersFlag(fs)
	tf := addToolFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 && fs.NArg() != 3 {
		fmt.Fprint(stderr, providerUsage)
		return 2
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	pkgs, err := external.Discover(dir, t.pomeCache)
	if err != nil {
		return report(stderr, err)
	}
	i := slices.IndexFunc(pkgs, func(p external.Package) bool { return p.Provider == name })
	if i < 0 {
		return report(stderr, fmt.Errorf("no dependency of %s provides %q (add the provider's Pome to Pome.toml)", dir, name))
	}
	pkg := pkgs[i]
	exe, err := host.InspectBytes(pkg.Source+" ("+pkg.Provider+")", pkg.Program)
	if err != nil {
		return report(stderr, err)
	}
	if exe.Format == host.FormatNative {
		return report(stderr, fmt.Errorf("provider %s: a provider from a Pome must be WASM", name))
	}
	s, err := host.Start(ctx, exe, host.Options{Wasmtime: t.toolchain.Wasmtime, CallTimeout: time.Minute})
	if err != nil {
		return report(stderr, err)
	}
	defer s.Close()
	m := s.Manifest()
	if fs.NArg() == 2 {
		if len(m.Scripts) == 0 {
			fmt.Fprintf(stdout, "provider %s writes no scripts\n", name)
		}
		for _, sc := range m.Scripts {
			fmt.Fprintf(stdout, "%-12s %s\n", sc.Name, sc.Description)
		}
		return 0
	}
	cfg, err := external.LoadConfig(*providersFile)
	if err != nil {
		return report(stderr, err)
	}
	settings, creds, err := cfg.ScriptInput(name)
	if err != nil {
		return report(stderr, err)
	}
	text, err := s.Script(ctx, protocol.ScriptParams{Name: fs.Arg(2), Settings: settings, Credentials: creds})
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stderr, "script %s of provider %s %s (%s, %s@%s), from %s: review every line before running it\n",
		fs.Arg(2), name, m.Version, pkg.Digest, pkg.Source, pkg.Version, *providersFile)
	fmt.Fprint(stdout, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(stdout)
	}
	return 0
}
