package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/plan"
)

// writeUses prints privileges grouped by path; "?" marks those used only in some cases.
func writeUses(w io.Writer, indent string, uses []lab.Use) bool {
	maybe := false
	var paths []string
	byPath := map[string][]string{}
	for _, u := range uses {
		if _, ok := byPath[u.Path]; !ok {
			paths = append(paths, u.Path)
		}
		p := u.Priv
		if u.Maybe {
			p += "?"
			maybe = true
		}
		byPath[u.Path] = append(byPath[u.Path], p)
	}
	for i, p := range paths {
		label := "uses "
		if i > 0 {
			label = "     "
		}
		fmt.Fprintf(w, "%s%s%s: %s\n", indent, label, p, strings.Join(byPath[p], ", "))
	}
	return maybe
}

func writeOpUses(w io.Writer, indent string, u lab.OpUses) bool {
	switch {
	case u.Unsupported != "":
		fmt.Fprintf(w, "%snot carried out: %s\n", indent, u.Unsupported)
	case u.Mamelet != "":
		fmt.Fprintf(w, "%suses %s\n", indent, u.Mamelet)
	default:
		return writeUses(w, indent, u.Proxmox)
	}
	return false
}

// renderPlanUses says, for each change, which Proxmox privileges (or mamelet actions) applying the
// plan would use, concretely for the site.
func renderPlanUses(w io.Writer, site *lab.Site, p *plan.Plan, g *graph.Graph) {
	fmt.Fprintln(w, "\nPermissions this plan uses (operating token; from the permission catalog, `mamela lab permissions` explains each):")
	maybe := writeUses(w, "  observing   ", site.ObserveUses())
	for _, cu := range site.PlanUses(p, g) {
		fmt.Fprintf(w, "  %s\n", cu.Change.ID())
		for _, u := range cu.Ops {
			fmt.Fprintf(w, "    %s\n", u.Op)
			if writeOpUses(w, "      ", u) {
				maybe = true
			}
		}
	}
	if maybe {
		fmt.Fprintln(w, "  ? used only in some cases (for example an upload that is already done)")
	}
}

// renderMayNeed says what the declaration's kinds of resources may need, whatever the world is.
func renderMayNeed(w io.Writer, site *lab.Site, g *graph.Graph) {
	fmt.Fprintln(w, "\nMay need when applied (by kind of resource; `mamela plan --site` says what a plan actually uses):")
	maybe := writeUses(w, "  observing   ", site.ObserveUses())
	for _, ku := range site.MayNeed(g) {
		fmt.Fprintf(w, "  %s\n", ku.Kind)
		for _, u := range ku.Ops {
			fmt.Fprintf(w, "    %s\n", u.Op)
			if writeOpUses(w, "      ", u) {
				maybe = true
			}
		}
	}
	if maybe {
		fmt.Fprintln(w, "  ? used only in some cases")
	}
}
