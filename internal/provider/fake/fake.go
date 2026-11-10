// Package fake is an in-memory Provider for tests and local development. It models a small world
// of VMs, packages and services, counts every call, and can inject failures: an error before the
// change, or a change whose response is lost.
package fake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
)

// Fault is an injected failure for the next call of one operation.
type Fault int

const (
	// FailBefore returns an error without changing anything.
	FailBefore Fault = iota + 1
	// LoseResponse makes the change, then reports an unknown outcome.
	LoseResponse
)

// World is the fake real world.
type World struct {
	mu       sync.Mutex
	clock    func() time.Time
	vms      map[string]state.VM
	packages map[string]state.Package
	services map[string]state.Service
	// Catalog lists the "<node>/<name>" packages and services the fake agent reports on each
	// managed VM, as a real agent reports what its revision declares.
	catalog []string
	faults  map[policy.Op][]Fault
	calls   map[policy.Op]int
	// Duplicates counts create-vm calls for a VM that already existed (the real API refuses them).
	Duplicates int
}

// New returns an empty world. catalog lists the "<node>/<name>" entries the agent reports.
func New(clock func() time.Time, catalog ...string) *World {
	return &World{
		clock: clock, catalog: catalog,
		vms: map[string]state.VM{}, packages: map[string]state.Package{}, services: map[string]state.Service{},
		faults: map[policy.Op][]Fault{}, calls: map[policy.Op]int{},
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

// SetVM changes a VM directly, as a person or another tool would.
func (w *World) SetVM(name string, vm state.VM) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.vms[name] = vm
}

// SetService changes a service directly (for example, someone stops nginx by hand).
func (w *World) SetService(key string, s state.Service) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.services[key] = s
}

// Observe returns a snapshot of the world as of now.
func (w *World) Observe(ctx context.Context) (state.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock().UTC()
	snap := state.Empty(now)
	for name, vm := range w.vms {
		vm.ObservedAt = now
		snap.VMs[name] = vm
	}
	for _, key := range w.catalog {
		node := nodeOf(key)
		if vm, ok := w.vms[node]; !ok || !vm.Managed {
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
	case policy.OpCreateVM:
		if _, exists := w.vms[r.Name]; exists {
			w.Duplicates++
			return fmt.Errorf("VM %s already exists", r.Name)
		}
		s := r.VirtualMachine
		w.vms[r.Name] = state.VM{Managed: true, CPU: s.CPU, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB, Image: s.Image}
	case policy.OpResizeVMCPU, policy.OpResizeVMMemory, policy.OpGrowVMDisk:
		vm, ok := w.vms[r.Name]
		if !ok {
			return fmt.Errorf("VM %s does not exist", r.Name)
		}
		s := r.VirtualMachine
		switch step.Op {
		case policy.OpResizeVMCPU:
			vm.CPU = s.CPU
		case policy.OpResizeVMMemory:
			vm.MemoryMiB = s.MemoryMiB
		case policy.OpGrowVMDisk:
			vm.DiskGiB = s.DiskGiB
		}
		w.vms[r.Name] = vm
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
