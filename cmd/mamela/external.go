package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"

	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
)

// defaultSite is where the site definition is looked for (gitignored; mamela.site.example.toml).
const defaultSite = "mamela.site.toml"

func addSiteFlag(fs *flag.FlagSet) *string {
	return fs.String("site", envOr("MAMELA_SITE", defaultSite),
		"the site definition: the Control Plane, the machines' login, network rules and limits (TOML; see mamela.site.example.toml)")
}

// loadSite reads the site definition, or returns nil when there is none at the default place (a
// declaration of providers' resources alone needs none). A site named explicitly must exist.
func loadSite(path string) (*sitedef.Site, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) && path == defaultSite && os.Getenv("MAMELA_SITE") == "" {
		return nil, nil
	}
	return sitedef.Load(path)
}

func addProvidersFlag(fs *flag.FlagSet) *string {
	return fs.String("providers", envOr("MAMELA_PROVIDERS", "mamela.providers.toml"),
		"where the APIs of the declaration's providers are, and how to authenticate (TOML; see docs/provider-authoring.md)")
}

// startProviders finds the providers among the dependencies of the Pome in dir and starts them,
// isolated. nil when there are none.
func startProviders(ctx context.Context, dir string, t tools, configPath string) (*external.Runtime, error) {
	pkgs, err := external.Discover(dir, t.pomeCache)
	if err != nil || len(pkgs) == 0 {
		return nil, err
	}
	return startPrograms(ctx, external.Programs(pkgs), t, configPath)
}

// declaredProviders are the built-in providers and those the declaration in dir imports: their
// manifests say what each resource may need. Nothing is observed; no API is contacted.
func declaredProviders(ctx context.Context, site *sitedef.Site, dir string, t tools) (spec.Providers, error) {
	rt, err := startProviders(ctx, dir, t, "")
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	return append(site.Providers(), rt.Manifests()...), nil
}

// startPrograms starts the providers a plan was made with (stored with the plan).
func startPrograms(ctx context.Context, programs []store.Program, t tools, configPath string) (*external.Runtime, error) {
	if len(programs) == 0 {
		return nil, nil
	}
	cfg, err := external.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	return external.Start(ctx, programs, cfg, t.toolchain.Wasmtime)
}
