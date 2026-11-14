// Package tfimport reads the resources of a Terraform or OpenTofu state, as `terraform show -json`
// (or `tofu show -json`) prints it (ADR 0009). Mamela never runs Terraform and never reads its
// state file directly: a person runs show and gives Mamela its output.
package tfimport

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

type show struct {
	FormatVersion string `json:"format_version"`
	Values        *struct {
		RootModule module `json:"root_module"`
	} `json:"values"`
}

type module struct {
	Address      string     `json:"address"`
	Resources    []resource `json:"resources"`
	ChildModules []module   `json:"child_modules"`
}

type resource struct {
	Address string         `json:"address"`
	Mode    string         `json:"mode"`
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Index   jsontext.Value `json:"index"`
	Values  jsontext.Value `json:"values"`
}

// Read returns the managed resources of a state (data sources are not resources), in the order of
// their addresses, each with the name Mamela proposes for its declaration: its modules, its name
// and its index, joined by "-" ("module.net.aws_vpc.main[0]": "net-main-0"). Two resources of one
// type never get the same name.
func Read(data []byte) ([]protocol.ImportObject, error) {
	var s show
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("not the JSON of `terraform show -json`: %w", err)
	}
	if !strings.HasPrefix(s.FormatVersion, "1.") {
		return nil, fmt.Errorf("format_version %q: Mamela reads the JSON of `terraform show -json` (format 1.x) of a state, "+
			"not a state file or a plan", s.FormatVersion)
	}
	if s.Values == nil {
		return nil, errors.New("the state has no resources (is it empty, or the JSON of a plan?)")
	}
	var all []resource
	var walk func(m module)
	walk = func(m module) {
		all = append(all, m.Resources...)
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	walk(s.Values.RootModule)
	slices.SortFunc(all, func(a, b resource) int { return strings.Compare(a.Address, b.Address) })
	var out []protocol.ImportObject
	used := map[string]bool{}
	for _, r := range all {
		if r.Mode != "managed" {
			continue
		}
		if r.Type == "" || r.Address == "" || len(r.Values) == 0 || r.Values.Kind() != '{' {
			return nil, fmt.Errorf("resource %q: no type, address or values", r.Address)
		}
		name := proposeName(r)
		base := name
		for i := 2; used[r.Type+" "+name]; i++ {
			name = base + "-" + strconv.Itoa(i)
		}
		used[r.Type+" "+name] = true
		out = append(out, protocol.ImportObject{Address: r.Address, Type: r.Type, Name: name, Values: r.Values})
	}
	return out, nil
}

var (
	moduleRe = regexp.MustCompile(`module\.([^.\[]+)(\[[^\]]*\])?`)
	unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

func proposeName(r resource) string {
	var parts []string
	// The modules the resource is in, from its address.
	if prefix, ok := strings.CutSuffix(r.Address, "."+r.Type+"."+r.Name+indexSuffix(r.Index)); ok {
		for _, m := range moduleRe.FindAllStringSubmatch(prefix, -1) {
			parts = append(parts, m[1])
			if m[2] != "" {
				parts = append(parts, strings.Trim(m[2], `[]"`))
			}
		}
	}
	parts = append(parts, r.Name)
	if len(r.Index) > 0 && string(r.Index) != "null" {
		var s string
		if json.Unmarshal(r.Index, &s) != nil {
			s = string(r.Index)
		}
		parts = append(parts, s)
	}
	name := strings.Trim(unsafeRe.ReplaceAllString(strings.Join(parts, "-"), "-"), "-._")
	if name == "" {
		name = r.Type
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func indexSuffix(index jsontext.Value) string {
	if len(index) == 0 || string(index) == "null" {
		return ""
	}
	return "[" + string(index) + "]"
}
