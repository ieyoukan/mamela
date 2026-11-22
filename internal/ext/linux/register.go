package linux

import (
	"context"
	"encoding/json/jsontext"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

// kinds are the extension's kinds, in canonical order. A kind declares what it adds to each layer
// (fileKind in file.go) and adds itself here, in the order doc.go reserves for it.
var kinds = []kind{
	userKind,        // 6: user.go
	aptRepoKind,     // 7: aptrepo.go
	artifactKind,    // 8: artifact.go
	fileKind,        // 9: file.go
	aptUpgradesKind, // 10: upgrades.go
}

// kind is what one kind of the extension adds to each layer. Every function works on its own
// members of Part and Observed, and leaves the others' alone.
type kind struct {
	def graph.KindDef
	// claims is the node and the path a resource writes (ok false: none): no two resources on a node
	// claim one path, nor one a directory above another's (graph.go).
	claims func(r graph.Resource) (node, path string, ok bool)
	// content is how many bytes of the node's revision an approved write of the resource takes.
	content func(r graph.Resource) int
	// warnings are the kind's warnings of the graph (in the canonical order of its resources).
	warnings func(g *graph.Graph) []graph.Warning
	// warningCodes are the codes of those warnings, and codes the codes of the kind's plan notes:
	// each is the extension's alone, in the ranges reserved for the kind (codes.go).
	warningCodes []graph.Code
	codes        []string
	// programs are the programs mamelet runs for the kind (absolute paths), besides systemctl.
	programs []string

	planner    plan.Planner
	startGate  func(k *plan.Kit, node, service string) *plan.Note
	undeclared func(k *plan.Kit) []plan.Change
	// packageGate says what the installation of a package on a node waits for (nil: nothing).
	packageGate func(k *plan.Kit, node, pkg string) *plan.Note

	ops      []operation
	manifest spec.Kind

	// The node's revision (the Control Plane): what a declaration adds to the part (publish), what it
	// no longer has (prune), and every grant out of the part of a protected node (protect).
	publish func(g *graph.Graph, node string, p *Part)
	prune   func(g *graph.Graph, node string, p *Part)
	protect func(p *Part)

	// mamelet: what it observes of the part, the grants it carries out (it says whether it changed
	// the system), and their recorded outcomes.
	observe func(ctx context.Context, s *mamelet.System, p Part, o *Observed)
	act     func(ctx context.Context, x *mamelet.Act, p Part) bool
	results func(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result
}

// operation is an operation of a kind: its decision (always an approval: policy.RegisterOp) and its
// grant.
type operation struct {
	op       policy.Op
	decision policy.Decision
	grant    node.GrantFunc
}

func init() {
	var defs []graph.KindDef
	planners := map[graph.Kind]plan.Planner{}
	grants := map[policy.Op]node.GrantFunc{}
	var manifest []spec.Kind
	var wcodes []graph.Code
	var codes []string
	var programs []string
	for _, k := range kinds {
		defs = append(defs, k.def)
		planners[k.def.Kind] = k.planner
		manifest = append(manifest, k.manifest)
		for _, o := range k.ops {
			policy.RegisterOp(Name, o.op, o.decision)
			grants[o.op] = o.grant
		}
		wcodes, codes, programs = append(wcodes, k.warningCodes...), append(codes, k.codes...), append(programs, k.programs...)
	}
	codes = append(codes, codeNoRoom) // every kind's (targets.go)
	graph.Register(graph.Extension{Name: Name, Kinds: defs, Check: checkGraph, Warnings: graphWarnings, WarningCodes: wcodes})

	plan.Register(plan.Extension{
		Name:     Name,
		Planners: planners,
		StartGate: func(k *plan.Kit, node, service string) *plan.Note {
			for _, kd := range kinds {
				if kd.startGate != nil {
					if n := kd.startGate(k, node, service); n != nil {
						return n
					}
				}
			}
			return nil
		},
		PackageGate: func(k *plan.Kit, node, pkg string) *plan.Note {
			for _, kd := range kinds {
				if kd.packageGate != nil {
					if n := kd.packageGate(k, node, pkg); n != nil {
						return n
					}
				}
			}
			return nil
		},
		Undeclared: func(k *plan.Kit) []plan.Change {
			var out []plan.Change
			for _, kd := range kinds {
				if kd.undeclared != nil {
					out = append(out, kd.undeclared(k)...)
				}
			}
			return out
		},
		Codes: codes,
	})

	noderev.RegisterPart(Name, noderev.Part{Version: Version, Grants: partGrants, ForMamelet: partForMamelet})

	node.Register(node.Extension{
		Name:  Name,
		Wire:  Version,
		Kinds: manifest,
		Annotate: func(_ string, rev *noderev.Revision, o *state.ExtObservation) {
			annotate(rev, o)
		},
		Publish: func(_ context.Context, _ *store.Store, g *graph.Graph, name string) (func(r *noderev.Revision), error) {
			return func(r *noderev.Revision) {
				p := partOf(r)
				for _, k := range kinds {
					k.publish(g, name, &p)
				}
				setPart(r, p)
			}, nil
		},
		Protect: func(r *noderev.Revision) {
			if _, ok := r.Ext[Name]; !ok {
				return
			}
			p := partOf(r)
			for _, k := range kinds {
				k.protect(&p)
			}
			setPart(r, p)
		},
		Prune: func(g *graph.Graph, name string, r *noderev.Revision) {
			if _, ok := r.Ext[Name]; !ok {
				return
			}
			p := partOf(r)
			for _, k := range kinds {
				k.prune(g, name, &p)
			}
			setPart(r, p)
		},
		Grants: grants,
	})

	mamelet.Register(mamelet.Extension{
		Name:     Name,
		Wire:     Version,
		Check:    checkPart,
		Programs: programs,
		Observe: func(ctx context.Context, s *mamelet.System, rev *noderev.Revision, rep *noderev.Report) {
			raw, ok := rev.Ext[Name]
			if !ok {
				return
			}
			var p Part
			if decodeStrict(raw, &p) != nil {
				return // Check refused it already
			}
			left := p.fit()
			var o Observed
			for _, k := range kinds {
				k.observe(ctx, s, p, &o)
			}
			left.add(&o)
			noteAptFailures(p, rep.Results, &o)
			noteUpgradeFailures(p, rep.Results, &o) // NodeAptUpgrades
			observePending(s, &o)
			cleanStateDir(s)
			o.SetAside = setAsideRecords(stateDir(s))
			if rep.Ext == nil {
				rep.Ext = map[string]jsontext.Value{}
			}
			rep.Ext[Name] = encodeObserved(o)
		},
		Act: func(ctx context.Context, x *mamelet.Act, phase mamelet.Phase) bool {
			// After the packages (a package may make the user that owns a file, or the directory), before
			// mamelet's own update and the services (a service starts with its files in place).
			raw, ok := x.Revision.Ext[Name]
			if !ok || phase != mamelet.BeforeUpdate {
				return false
			}
			var p Part
			if decodeStrict(raw, &p) != nil {
				return false
			}
			changed := false
			for _, k := range kinds {
				changed = k.act(ctx, x, p) || changed
			}
			return changed
		},
		Results: func(rev *noderev.Revision, phase mamelet.Phase, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
			raw, ok := rev.Ext[Name]
			if !ok || phase != mamelet.BeforeUpdate {
				return nil
			}
			var p Part
			if decodeStrict(raw, &p) != nil {
				return nil
			}
			var out []noderev.Result
			for _, k := range kinds {
				out = append(out, k.results(p, known)...)
			}
			return out
		},
	})
}
