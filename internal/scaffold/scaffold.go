// Package scaffold writes a new provider's repository (mamela provider new, ADR 0009): a provider
// written with provider/kit against a fake of its API, the API's catalog, the conformance test,
// the Pome with its generated Emela, build.sh and CI. What it writes passes its tests as written,
// so that an author starts from something that works and changes it step by step.
package scaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"unicode"

	"github.com/ieyoukan/mamela/internal/pomegen"
	"github.com/ieyoukan/mamela/provider/protocol"
)

//go:embed template
var files embed.FS

// Options are what the author chooses.
type Options struct {
	// GoModule is the Go module path ("github.com/me/mamela-foo").
	GoModule string
	// Provider is the provider's name ("foo"): lower case letters, digits and -.
	Provider string
	// Module is the Pome's Emela module ("Foo"; default: from Provider).
	Module string
	// API is the API's name ("foo/v1"; default: Provider + "/v1").
	API string
	// MamelaVersion is the version of github.com/ieyoukan/mamela go.mod requires.
	MamelaVersion string
	// Mamela is a local checkout of Mamela to use instead of published versions (Go replace and
	// the Pome's [replace]), until they are published; relative paths are written.
	Mamela string
}

type data struct {
	GoModule, Provider, Pkg, Module, Prefix, API string
	ThingType, ThingFn                           string
	PomeName, CatalogName, CatalogFile           string
	MamelaVersion, MamelaGo, MamelaPome          string
}

var (
	providerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	goModule     = regexp.MustCompile(`^[A-Za-z0-9.-]+(/[A-Za-z0-9._~-]+)+$`)
)

// Write writes the repository into dir, which must not exist or be empty.
func Write(dir string, o Options) ([]string, error) {
	if !providerName.MatchString(o.Provider) {
		return nil, fmt.Errorf("provider name %q: lower case letters, digits and -, starting with a letter", o.Provider)
	}
	if !goModule.MatchString(o.GoModule) {
		return nil, fmt.Errorf("Go module path %q: like github.com/you/mamela-%s", o.GoModule, o.Provider)
	}
	d := data{GoModule: o.GoModule, Provider: o.Provider, Pkg: strings.ReplaceAll(o.Provider, "-", ""), Module: o.Module, API: o.API,
		MamelaVersion: o.MamelaVersion}
	if d.Module == "" {
		d.Module = camel(o.Provider)
	}
	if !regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`).MatchString(d.Module) {
		return nil, fmt.Errorf("module %q: an Emela module name (UpperCamelCase)", d.Module)
	}
	if d.API == "" {
		d.API = o.Provider + "/v1"
	}
	if d.MamelaVersion == "" {
		return nil, errors.New("no version of Mamela to require")
	}
	d.Prefix = d.Module
	d.ThingType, d.ThingFn = pomegen.Names(protocol.ResourceKind(d.Prefix + "Thing"))
	d.PomeName = o.GoModule + "/pome"
	d.CatalogName = d.Pkg
	d.CatalogFile = d.Pkg + ".json"
	if o.Mamela != "" {
		// Both resolved (macOS's /tmp is /private/tmp), so that the relative paths hold.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		abs, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, err
		}
		m, err := filepath.EvalSymlinks(o.Mamela)
		if err != nil {
			return nil, err
		}
		if abs, err = filepath.Abs(abs); err != nil {
			return nil, err
		}
		if m, err = filepath.Abs(m); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(m, "provider/kit")); err != nil {
			return nil, fmt.Errorf("--mamela %s is not a checkout of Mamela (no provider/kit)", o.Mamela)
		}
		if d.MamelaGo, err = filepath.Rel(abs, m); err != nil {
			return nil, err
		}
		if d.MamelaPome, err = filepath.Rel(filepath.Join(abs, "pome"), filepath.Join(m, "pome/mamela")); err != nil {
			return nil, err
		}
		d.MamelaGo, d.MamelaPome = filepath.ToSlash(d.MamelaGo), filepath.ToSlash(d.MamelaPome)
		if !strings.HasPrefix(d.MamelaGo, ".") {
			d.MamelaGo = "./" + d.MamelaGo
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty: write a new provider into a new directory", dir)
	}
	var written []string
	err := fs.WalkDir(files, "template", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		src, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(p).Delims("<%", "%>").Option("missingkey=error").Parse(string(src))
		if err != nil {
			return err
		}
		var out bytes.Buffer
		if err := t.Execute(&out, d); err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "template/"), ".tmpl")
		rel = strings.NewReplacer("PKG", d.Pkg, "CATALOG", d.CatalogFile).Replace(rel)
		switch {
		case rel == "gitignore":
			rel = ".gitignore"
		case strings.HasPrefix(rel, "github/"):
			rel = "." + rel
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if path.Ext(rel) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(target, out.Bytes(), mode); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	return written, err
}

func camel(s string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '-' }) {
		rs := []rune(w)
		rs[0] = unicode.ToUpper(rs[0])
		b.WriteString(string(rs))
	}
	return b.String()
}
