package external

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/store"
)

// Pins (ADR 0015). A declaration's tree names the Pomes it imports, and so chooses the program of a
// provider and the catalog that classifies its requests; both run with the operator's credentials,
// during plan too. Only the operator may choose them: mamela.providers.toml pins, for a provider,
// the sha256 of its program (program_sha256) and, for each API whose catalog is one a Pome ships,
// the sha256 of that catalog (catalog_sha256). A program or catalog that is not the pinned one does
// not start.
//
// A provider without a pin runs as before and says so (MAM-PRV-0010), until the operator writes
// strict_pins = true, which refuses it.
const (
	pinMismatch = "MAM-PRV-0009" // the program or catalog is not the pinned one
	pinMissing  = "MAM-PRV-0010" // a provider is not pinned (a warning; an error with strict_pins)
)

// section is one place of mamela.providers.toml that has an API: the provider's own, or one of its
// [provider.<name>.apis.<api>].
type section struct {
	label string // "[provider.kv-example]" or "[provider.aws.apis.ec2]"
	api   string
	cfg   APIConfig
}

// sections are the sections of a configured provider that say where an API is, in a fixed order.
func (c Config) sections(name string) []section {
	a, ok := c.Provider[name]
	if !ok {
		return nil
	}
	if len(a.APIs) == 0 {
		return []section{{"[provider." + name + "]", "", a}}
	}
	var out []section
	for _, api := range slices.Sorted(maps.Keys(a.APIs)) {
		out = append(out, section{"[provider." + name + ".apis." + api + "]", api, a.inherit(a.APIs[api])})
	}
	return out
}

// pomeCatalog is the name of the catalog a Pome ships that a section names ("pome:ec2" is "ec2").
func pomeCatalog(ref string) (string, bool) { return strings.CutPrefix(ref, "pome:") }

// sum is the sha256 of data in hex.
func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// normalizeDigest reads a pin, "sha256:<hex>" or plain hex, as lowercase hex.
func normalizeDigest(key, v string) (string, error) {
	h := strings.ToLower(strings.TrimPrefix(v, "sha256:"))
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("%s %q is not a sha256 (64 hexadecimal digits, as `mamela provider pin` prints it)", key, v)
	}
	return h, nil
}

// checkPinSettings checks the pins one provider's sections carry, and with strict_pins that the
// provider has them.
func (c *Config) checkPinSettings(path, name string) error {
	a := c.Provider[name]
	var err error
	if a.ProgramSHA256 != "" {
		if a.ProgramSHA256, err = normalizeDigest("program_sha256", a.ProgramSHA256); err != nil {
			return fmt.Errorf("%s: provider %s: %w", path, name, err)
		}
	}
	if a.CatalogSHA256 != "" {
		if a.CatalogSHA256, err = normalizeDigest("catalog_sha256", a.CatalogSHA256); err != nil {
			return fmt.Errorf("%s: provider %s: %w", path, name, err)
		}
	}
	for api, sub := range a.APIs {
		if sub.ProgramSHA256 != "" {
			return fmt.Errorf("%s: provider %s api %s: program_sha256 pins the provider's program: it belongs to the provider's section", path, name, api)
		}
		if sub.CatalogSHA256 != "" {
			if sub.CatalogSHA256, err = normalizeDigest("catalog_sha256", sub.CatalogSHA256); err != nil {
				return fmt.Errorf("%s: provider %s api %s: %w", path, name, api, err)
			}
			a.APIs[api] = sub
		}
	}
	c.Provider[name] = a
	for _, s := range c.sections(name) {
		_, shipped := pomeCatalog(s.cfg.Catalog)
		if s.cfg.CatalogSHA256 != "" && !shipped {
			return fmt.Errorf("%s: provider %s: catalog_sha256 in %s pins a catalog a provider's Pome ships (catalog = \"pome:<name>\"); "+
				"a file of your own is not something a declaration can change", path, name, s.label)
		}
	}
	if !c.StrictPins {
		return nil
	}
	if missing := c.unpinned(name); len(missing) > 0 {
		return fmt.Errorf("%s: %s: provider %s is not pinned (%s), and strict_pins = true refuses a provider whose program and catalog a declaration could choose\n"+
			"  hint: run `mamela provider pin <declaration-dir> %s` and put the lines it prints in %s", path, pinMissing, name, strings.Join(missing, "; "), name, path)
	}
	return nil
}

// unpinned says what of a configured provider is not pinned.
func (c Config) unpinned(name string) []string {
	a := c.Provider[name]
	var out []string
	if a.ProgramSHA256 == "" {
		out = append(out, "program_sha256 in [provider."+name+"]")
	}
	for _, s := range c.sections(name) {
		if _, shipped := pomeCatalog(s.cfg.Catalog); shipped && s.cfg.CatalogSHA256 == "" {
			out = append(out, "catalog_sha256 in "+s.label)
		}
	}
	return out
}

// checkPins checks a program (and the catalogs its Pome ships) against what the operator pinned,
// before anything of it runs. For a provider that is not pinned it returns a warning; with
// strict_pins, an error. A provider with no API configured sends nothing, and needs no pin.
func (c Config) checkPins(p store.Program) (warnings []string, err error) {
	a, ok := c.Provider[p.Provider]
	if !ok {
		return nil, nil
	}
	if want := a.ProgramSHA256; want != "" {
		if got := sum(p.Data); got != want {
			return nil, fmt.Errorf("%s: provider %s: the program of the declaration's Pome (%s, sha256 %s) is not the one pinned in mamela.providers.toml (program_sha256 %s)\n"+
				"  hint: a declaration chooses which Pome it imports, but only the operator chooses which program runs with the operator's credentials (ADR 0015). "+
				"If this is an upgrade you reviewed, run `mamela provider pin <declaration-dir> %s` on the Control Plane host and put the digests it prints in mamela.providers.toml; "+
				"if nobody meant to change the provider, find out who changed the declaration's Pome or Pome.lock", pinMismatch, p.Provider, p.Source, got, want, p.Provider)
		}
	}
	for _, s := range c.sections(p.Provider) {
		name, shipped := pomeCatalog(s.cfg.Catalog)
		if !shipped || s.cfg.CatalogSHA256 == "" {
			continue
		}
		data, ok := p.Catalogs[name]
		if !ok {
			continue // the catalog is not there: loading it says so
		}
		if got := sum(data); got != s.cfg.CatalogSHA256 {
			return nil, fmt.Errorf("%s: provider %s: the catalog %q of the declaration's Pome (%s, sha256 %s) is not the one pinned in %s (catalog_sha256 %s)\n"+
				"  hint: a catalog decides which requests the host lets through without a person's approval, and a declaration must not choose it (ADR 0015). "+
				"If you reviewed the new catalog (mamela provider catalog <pome-dir>), run `mamela provider pin <declaration-dir> %s` and update the pin; otherwise find out who changed it",
				pinMismatch, p.Provider, name, p.Source, got, s.label, s.cfg.CatalogSHA256, p.Provider)
		}
	}
	if missing := c.unpinned(p.Provider); len(missing) > 0 {
		if c.StrictPins {
			return nil, fmt.Errorf("%s: provider %s is not pinned (%s), and strict_pins = true refuses it", pinMissing, p.Provider, strings.Join(missing, "; "))
		}
		warnings = append(warnings, fmt.Sprintf("%s: provider %s is not pinned (%s): whoever can change the declaration's tree chooses the program and the catalog that run with this provider's credentials, even during plan\n"+
			"  hint: run `mamela provider pin <declaration-dir> %s` and put the lines it prints in mamela.providers.toml; strict_pins = true refuses unpinned providers (ADR 0015)",
			pinMissing, p.Provider, strings.Join(missing, "; "), p.Provider))
	}
	return warnings, nil
}

// PinLine is one line mamela.providers.toml should carry for a provider's package.
type PinLine struct {
	Section string // where it goes: "[provider.kv-example]"
	Key     string // program_sha256 or catalog_sha256
	Want    string // the package's digest, in hex
	Have    string // what the configuration says now; empty: not pinned
	What    string // what it pins
}

// State says whether the line is in the configuration: "pinned", "differs" or "not pinned".
func (l PinLine) State() string {
	switch {
	case l.Have == "":
		return "not pinned"
	case l.Have == l.Want:
		return "pinned"
	}
	return "differs"
}

// PinLines are the pins the package's program and catalogs call for, with what the configuration
// says of each. A provider the configuration does not have yet gets the lines of a provider with
// one API.
func (c Config) PinLines(pkg Package) []PinLine {
	what := "this Pome"
	if pkg.Source != "" {
		what = pkg.Source + "@" + pkg.Version
	}
	main := "[provider." + pkg.Provider + "]"
	have := c.Provider[pkg.Provider].ProgramSHA256
	out := []PinLine{{main, "program_sha256", strings.TrimPrefix(pkg.Digest, "sha256:"), have, "the program of " + what}}
	secs := c.sections(pkg.Provider)
	for _, name := range slices.Sorted(maps.Keys(pkg.Catalogs)) {
		line := PinLine{Section: `the section that says catalog = "pome:` + name + `"`, Key: "catalog_sha256", Want: sum(pkg.Catalogs[name]),
			What: `the catalog "` + name + `" of ` + what}
		if len(secs) == 0 {
			line.Section = main
		}
		for _, s := range secs {
			if n, shipped := pomeCatalog(s.cfg.Catalog); shipped && n == name {
				line.Section, line.Have = s.label, s.cfg.CatalogSHA256
			}
		}
		out = append(out, line)
	}
	return out
}
