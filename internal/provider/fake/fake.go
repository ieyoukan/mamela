// Package fake is an in-memory Provider for tests: a small world of machines of a provider's node
// kind (FakeVM of provider "fake", spec {"cpu": n}), and the packages and services on them. It
// computes a provider's changes for the declared machines as a real provider's diff would, counts
// every call, and can inject failures: an error before the change, or a change whose response is
// lost.
package fake

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The fake provider's kind and operations.
const (
	Provider                       = "fake"
	Kind     protocol.ResourceKind = "FakeVM"

	OpCreate protocol.Op = "create-vm" // created stopped
	OpStart  protocol.Op = "start-vm"  // late: after every create of the run
	OpResize protocol.Op = "resize-vm"
	OpShrink protocol.Op = "shrink-vm" // never carried out (unsupported)
)

// Fault is an injected failure for the next call of one operation.
type Fault int

const (
	// FailBefore returns an error without changing anything.
	FailBefore Fault = iota + 1
	// LoseResponse makes the change, then reports an unknown outcome.
	LoseResponse
)

// Machine is one fake machine.
type Machine struct {
	Owned   bool // created by Mamela (its record and marker); otherwise Foreign
	Running bool
	CPU     int64
	ID      int64 // the node's number (assigned at create when 0)
}

// World is the fake real world.
type World struct {
	mu       sync.Mutex
	clock    func() time.Time
	declared []graph.Resource
	machines map[string]Machine
	packages map[string]state.Package
	services map[string]state.Service
	// Catalog lists the "<node>/<name>" packages and services the fake agent reports on each
	// managed, running machine, as a real agent reports what its revision declares.
	catalog []string
	faults  map[policy.Op][]Fault
	calls   map[policy.Op]int
	// Duplicates counts creates of a machine that already existed (a real API refuses them).
	Duplicates int
	// Applied lists every operation applied, in order ("create-vm web").
	Applied []string
}

// New returns an empty world. catalog lists the "<node>/<name>" entries the agent reports.
func New(clock func() time.Time, catalog ...string) *World {
	return &World{
		clock: clock, catalog: catalog,
		machines: map[string]Machine{}, packages: map[string]state.Package{}, services: map[string]state.Service{},
		faults: map[policy.Op][]Fault{}, calls: map[policy.Op]int{},
	}
}

// Declare tells the fake provider the declaration (a real provider receives its resources).
func (w *World) Declare(g *graph.Graph) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.declared = nil
	for _, r := range g.Sorted() {
		if r.Provided != nil && r.Provided.Provider == Provider && r.Kind == Kind {
			w.declared = append(w.declared, r)
		}
	}
}

// Inject queues a fault for the next call of op.
func (w *World) Inject(op policy.Op, f Fault) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.faults[op] = append(w.faults[op], f)
}

// Calls returns how many times op was applied (including failed attempts).
func (w *World) Calls(op policy.Op) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[op]
}

// SetMachine changes a machine directly, as a person or another tool would.
func (w *World) SetMachine(name string, m Machine) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if m.ID == 0 {
		m.ID = int64(7000 + len(w.machines))
	}
	w.machines[name] = m
}

// SetService changes a service directly (for example, someone stops nginx by hand).
func (w *World) SetService(key string, s state.Service) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.services[key] = s
}

func cpuOf(r graph.Resource) int64 {
	var s struct {
		CPU int64 `json:"cpu"`
	}
	json.Unmarshal(r.Provided.Spec, &s)
	return s.CPU
}

// startReason is the same whether the start comes with a create or the machine is found stopped,
// so that after a create whose answer was lost the approved start is still the one to do.
const startReason = "start it (after every machine of the run is created)"

func version(m Machine) string { return "v" + strconv.FormatInt(m.CPU, 10) }

// Observe returns a snapshot of the world as of now, with the fake provider's changes for the
// declared machines.
func (w *World) Observe(ctx context.Context) (state.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock().UTC()
	snap := state.Empty(now)
	provided := map[string]state.Provided{}
	observe := func(name string, m Machine, exists bool) protocol.Observed {
		o := protocol.Observed{ID: string(Kind) + " " + name, Kind: Kind, Name: name, Exists: exists}
		if exists {
			o.Owned, o.Version, o.IDs = m.Owned, version(m), map[string]string{"name": name}
			o.Node = &protocol.ObservedNode{ID: m.ID, Running: m.Running}
		}
		return o
	}
	for _, r := range w.declared {
		m, exists := w.machines[r.Name]
		want := cpuOf(r)
		p := state.Provided{Provider: Provider, Kind: Kind, Name: r.Name, Declared: true, Observed: observe(r.Name, m, exists),
			SpecDigest: state.SpecDigest(r.Provided.Spec), ObservedAt: now}
		change := func(op protocol.Op, effect protocol.Effect, why string) state.ProvidedChange {
			c := protocol.Change{Resource: r.ID(), Kind: Kind, Op: op, IDs: map[string]string{"name": r.Name}, Reasons: []string{why},
				Requests: []protocol.Approved{{Method: "POST", Path: "/vms/" + r.Name + "/" + string(op), Params: map[string][]string{"cpu": {strconv.FormatInt(want, 10)}}}}}
			if exists && op != OpStart {
				c.Precondition = version(m) // a start depends on no version, as with Proxmox
			}
			return state.ProvidedChange{Change: c, Verified: protocol.Verified{Effects: []protocol.Effect{effect}}}
		}
		switch {
		case !exists:
			start := change(OpStart, protocol.EffectRestart, startReason)
			start.Change.Late, start.Change.Requests[0].Params = true, nil
			p.Changes = []state.ProvidedChange{change(OpCreate, protocol.EffectCreate, fmt.Sprintf("%d vCPU", want)), start}
		case !m.Owned:
		default:
			switch {
			case m.CPU < want:
				p.Changes = append(p.Changes, change(OpResize, protocol.EffectChange, fmt.Sprintf("vCPUs %d -> %d", m.CPU, want)))
			case m.CPU > want:
				c := change(OpShrink, protocol.EffectChange, fmt.Sprintf("vCPUs %d -> %d", m.CPU, want))
				c.Change.Requests, c.Unsupported = nil, "the fake never takes vCPUs away"
				p.Changes = append(p.Changes, c)
			}
			if !m.Running {
				start := change(OpStart, protocol.EffectRestart, startReason)
				start.Change.Late, start.Change.Requests[0].Params = true, nil
				p.Changes = append(p.Changes, start)
			}
		}
		provided[r.ID()] = p
	}
	// Machines Mamela created and that are no longer declared: reported, so that Mamela keeps them.
	for _, name := range slices.Sorted(maps.Keys(w.machines)) {
		id := string(Kind) + " " + name
		if _, ok := provided[id]; ok || !w.machines[name].Owned {
			continue
		}
		provided[id] = state.Provided{Provider: Provider, Kind: Kind, Name: name, Observed: observe(name, w.machines[name], true), ObservedAt: now}
	}
	snap.AddProvided(provided)

	for _, key := range w.catalog {
		if m, ok := w.machines[nodeOf(key)]; !ok || !m.Owned || !m.Running {
			continue // no agent there
		}
		p := w.packages[key]
		p.ObservedAt = now
		if !p.Installed {
			// What the agent's apt-get -s would report.
			cs := noderev.NewChangeSet([]noderev.Change{{Action: noderev.ChangeInstall, Package: pkgOf(key),
				Version: "1.0-fake", Origins: []string{"Ubuntu:24.04/noble"}}})
			p.ChangeSet = &cs
		}
		snap.Packages[key] = p
		s := w.services[key]
		s.Exists = s.Exists || p.Installed // installing the package brings the unit
		s.ObservedAt = now
		snap.Services[key] = s
	}
	return snap, nil
}

// Apply performs one step.
func (w *World) Apply(ctx context.Context, step reconcile.Step) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[step.Op]++
	w.Applied = append(w.Applied, string(step.Op)+" "+step.Resource.Name)
	var fault Fault
	if q := w.faults[step.Op]; len(q) > 0 {
		fault, w.faults[step.Op] = q[0], q[1:]
	}
	if fault == FailBefore {
		return errors.New("injected failure before the change")
	}
	if err := w.change(step); err != nil {
		return err
	}
	if fault == LoseResponse {
		return fmt.Errorf("injected lost response: %w", reconcile.ErrUnknownOutcome)
	}
	return nil
}

func (w *World) change(step reconcile.Step) error {
	r := step.Resource
	switch step.Op {
	case OpCreate:
		if _, exists := w.machines[r.Name]; exists {
			w.Duplicates++
			return fmt.Errorf("machine %s already exists", r.Name)
		}
		w.machines[r.Name] = Machine{Owned: true, CPU: cpuOf(r), ID: int64(7000 + len(w.machines))}
	case OpStart, OpResize:
		m, ok := w.machines[r.Name]
		if !ok {
			return fmt.Errorf("machine %s does not exist", r.Name)
		}
		if step.Op == OpStart {
			m.Running = true
		} else {
			m.CPU = cpuOf(r)
		}
		w.machines[r.Name] = m
	case policy.OpInstallPackage:
		w.packages[r.Name] = state.Package{Installed: true, Version: "1.0-fake"}
	case policy.OpEnableService, policy.OpDisableService, policy.OpStartService, policy.OpStopService:
		s := w.services[r.Name]
		switch step.Op {
		case policy.OpEnableService:
			s.Enabled = true
		case policy.OpDisableService:
			s.Enabled = false
		case policy.OpStartService:
			s.Running = true
		case policy.OpStopService:
			s.Running = false
		}
		w.services[r.Name] = s
	default:
		// The fake refuses anything it does not model, like a real provider would.
		return fmt.Errorf("operation %s is not supported", step.Op)
	}
	return nil
}

func pkgOf(key string) string {
	_, pkg, _ := strings.Cut(key, "/")
	return pkg
}

func nodeOf(key string) string {
	node, _, _ := strings.Cut(key, "/")
	return node
}
