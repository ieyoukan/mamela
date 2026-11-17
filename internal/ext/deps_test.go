// Package ext_test checks the line between the core and the node extensions (docs/design/
// node-extensions.md): the core's packages do not depend on any extension, and the programs Mamela
// ships are built with them.
package ext_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/ieyoukan/mamela"

// deps lists, for each package of the patterns, the packages it depends on (its tests left out).
func deps(t *testing.T, patterns ...string) map[string][]string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to list the packages with")
	}
	args := append([]string{"list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}"}, patterns...)
	cmd := exec.Command(gobin, args...)
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	m := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		m[f[0]] = f[1:]
	}
	return m
}

// No package of the core imports an extension, directly or not: an extension registers itself,
// and only the programs choose which are in.
func TestCoreDoesNotDependOnExtensions(t *testing.T) {
	all := deps(t, "./internal/...", "./provider/...")
	core := 0
	for pkg, ds := range all {
		if strings.HasPrefix(pkg, module+"/internal/ext/") {
			continue
		}
		core++
		for _, d := range ds {
			if strings.HasPrefix(d, module+"/internal/ext/") {
				t.Errorf("%s depends on the extension %s", pkg, d)
			}
		}
	}
	for _, must := range []string{"plan", "graph", "policy", "state", "noderev", "store", "provider/node", "provider/spec", "mamelet", "reconcile"} {
		if _, ok := all[module+"/internal/"+must]; !ok {
			t.Errorf("the core package internal/%s was not checked", must)
		}
	}
	if core < 20 {
		t.Errorf("only %d core packages were checked", core)
	}
}

// mamela and mamelet are built with every extension (k3s, linux): a Control Plane that plans an
// extension's kinds and a mamelet that does not carry them out (or the other way round) would not
// work together.
func TestProgramsAreBuiltWithTheExtensions(t *testing.T) {
	for pkg, ds := range deps(t, "./cmd/mamela", "./cmd/mamelet") {
		for _, ext := range []string{"k3s", "linux"} {
			if !slices.Contains(ds, module+"/internal/ext/"+ext) {
				t.Errorf("%s is built without internal/ext/%s", pkg, ext)
			}
		}
	}
}

// One extension does not depend on another: each registers with the core alone, and can move out
// of this repository by itself (docs/design/node-extensions.md, stage 6).
func TestExtensionsDoNotDependOnEachOther(t *testing.T) {
	for pkg, ds := range deps(t, "./internal/ext/k3s", "./internal/ext/linux") {
		for _, d := range ds {
			if strings.HasPrefix(d, module+"/internal/ext/") {
				t.Errorf("%s depends on the extension %s", pkg, d)
			}
		}
	}
}
