// Package pveprovider is a Mamela provider of Proxmox VE virtual machines that runs as a separate,
// isolated program (WASM): it reaches Proxmox only through the host, which holds the token and
// checks every request against Mamela's Proxmox catalog and the change's permit.
//
// Kind PveVM, spec {"node", "vmid", "pool", "storage", "cores", "memoryMiB", "diskGiB"}: a VM with
// an empty disk and no operating system. It creates VMs and changes their vCPUs and memory; it
// never deletes, starts or stops one, and never touches a VM without its ownership marker.
//
// The marker is in the VM's description, which can be set when the VM is created (Proxmox checks
// tags before the VM is in its pool, so a token limited to the pool cannot tag at creation).
package pveprovider

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// API is the API the provider talks to (Mamela's catalog "builtin:proxmox").
const API = "proxmox"

// Kind is the resource kind.
const Kind protocol.ResourceKind = "PveVM"

// Operations.
const (
	OpCreate protocol.Op = "create-vm"
	OpResize protocol.Op = "resize-vm"
	OpDelete protocol.Op = "delete-vm"
)

// Marker is the description of the VMs this provider created. A VM without it is never changed.
const Marker = "Managed by Mamela (pve-vm provider). Do not edit."

// Spec is the declared VM.
type Spec struct {
	Node      string `json:"node"`
	VMID      int64  `json:"vmid"`
	Pool      string `json:"pool"`
	Storage   string `json:"storage"`
	Cores     int64  `json:"cores"`
	MemoryMiB int64  `json:"memoryMiB"`
	DiskGiB   int64  `json:"diskGiB"`
}

// State is what is observed of a VM.
type State struct {
	Cores     int64 `json:"cores"`
	MemoryMiB int64 `json:"memoryMiB"`
}

var (
	listVMs   = protocol.Call{Method: "GET", Path: "/cluster/resources", Query: []string{"type"}}
	getConfig = protocol.Call{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config"}
	getTask   = protocol.Call{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"}
	createVM  = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"vmid", "name", "cores", "memory", "pool", "description", "scsi0", "scsihw", "ostype"}}
	setConfig = protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Form: []string{"cores", "memory", "digest"}}
)

// Provider implements sdk.Provider.
type Provider struct {
	// Poll is how long to wait between task status reads (default 1s).
	Poll time.Duration
}

// Manifest implements sdk.Provider.
func (Provider) Manifest() protocol.Manifest {
	acl := func(caps ...string) []protocol.Need {
		var out []protocol.Need
		for _, c := range caps {
			out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: "/pool/{pool}"}, Always: true})
		}
		return out
	}
	create := append(acl("VM.Allocate", "VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk", "VM.Config.HWType", "VM.Config.Options"),
		protocol.Need{Capability: "Datastore.AllocateSpace", Target: protocol.Target{Type: "proxmox-acl", Ref: "/storage/{storage}"}, Always: true})
	return protocol.Manifest{
		Schema: protocol.Schema, Provider: "pve-vm", Version: "0.1.0", API: API,
		Observe:      acl("VM.Audit"),
		ObserveCalls: []protocol.Call{listVMs, getConfig},
		Kinds: []protocol.Kind{{Kind: Kind, Operations: []protocol.Operation{
			{Op: OpCreate, Support: protocol.Supported, Needs: create, Calls: []protocol.Call{listVMs, getConfig, getTask, createVM}},
			{Op: OpResize, Support: protocol.Supported, Needs: acl("VM.Config.CPU", "VM.Config.Memory"), Calls: []protocol.Call{getConfig, setConfig}},
			{Op: OpDelete, Support: protocol.Unsupported, Why: "removing a declaration never deletes a VM"},
		}}},
	}
}

type resource struct {
	VMID   int64  `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Pool   string `json:"pool"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Observe implements sdk.Provider.
func (p Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	vms, err := list(ctx, h)
	if err != nil {
		return protocol.ObserveResult{}, err
	}
	var out protocol.ObserveResult
	declared := map[int64]bool{}
	for _, r := range in.Resources {
		s, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		declared[s.VMID] = true
		o := protocol.Observed{ID: r.ID, Kind: Kind, Name: r.Name, IDs: ids(s.Node, s.VMID)}
		if vm, ok := vms[s.VMID]; ok {
			if err := p.fill(ctx, h, &o, vm); err != nil {
				return out, err
			}
			o.Owned = o.Owned && vm.Name == r.Name
		}
		out.Resources = append(out.Resources, o)
	}
	for _, vmid := range slices.Sorted(maps.Keys(vms)) {
		if declared[vmid] {
			continue
		}
		vm := vms[vmid]
		o := protocol.Observed{ID: string(Kind) + "/" + vm.Name, Kind: Kind, Name: vm.Name, IDs: ids(vm.Node, vmid)}
		if err := p.fill(ctx, h, &o, vm); err != nil {
			return out, err
		}
		if o.Owned { // what it created and is no longer declared: reported, so that Mamela keeps it
			out.Resources = append(out.Resources, o)
		}
	}
	return out, nil
}

func (Provider) fill(ctx context.Context, h *sdk.Host, o *protocol.Observed, vm resource) error {
	cfg, err := config(ctx, h, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	o.Exists, o.Owned, o.Version = true, cfg.Description == Marker, cfg.Digest
	o.State, _ = json.Marshal(State{Cores: cfg.Cores, MemoryMiB: cfg.Memory}, json.Deterministic(true))
	return nil
}

func ids(node string, vmid int64) map[string]string {
	return map[string]string{"node": node, "vmid": strconv.FormatInt(vmid, 10)}
}

// Diff implements sdk.Provider: create what is missing; change vCPUs and memory of what it owns.
// A VM it does not own gets no change (Mamela denies changing it); a disk is never resized.
func (Provider) Diff(ctx context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		obs[o.ID] = o
	}
	var out protocol.DiffResult
	desired := slices.Clone(in.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	for _, r := range desired {
		s, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		o := obs[r.ID]
		c := protocol.Change{Resource: r.ID, Kind: Kind, IDs: ids(s.Node, s.VMID)}
		switch {
		case !o.Exists:
			c.Op = OpCreate
			c.Reasons = []string{fmt.Sprintf("VM %d does not exist: %d vCPU, %d MiB, an empty %d GiB disk on %s, in pool %s", s.VMID, s.Cores, s.MemoryMiB, s.DiskGiB, s.Storage, s.Pool)}
			c.Requests = []protocol.Approved{{Method: createVM.Method, Path: createVM.Path, Params: map[string][]string{
				"vmid": {strconv.FormatInt(s.VMID, 10)}, "name": {r.Name}, "cores": {strconv.FormatInt(s.Cores, 10)},
				"memory": {strconv.FormatInt(s.MemoryMiB, 10)}, "pool": {s.Pool}, "description": {Marker},
				"scsi0": {fmt.Sprintf("%s:%d", s.Storage, s.DiskGiB)}, "scsihw": {"virtio-scsi-single"}, "ostype": {"l26"}}}}
		case !o.Owned:
			continue
		default:
			var got State
			if err := json.Unmarshal(o.State, &got); err != nil {
				return out, err
			}
			params := map[string][]string{"digest": {o.Version}}
			if got.Cores != s.Cores {
				params["cores"] = []string{strconv.FormatInt(s.Cores, 10)}
				c.Reasons = append(c.Reasons, fmt.Sprintf("vCPUs %d -> %d", got.Cores, s.Cores))
			}
			if got.MemoryMiB != s.MemoryMiB {
				params["memory"] = []string{strconv.FormatInt(s.MemoryMiB, 10)}
				c.Reasons = append(c.Reasons, fmt.Sprintf("memory %d -> %d MiB", got.MemoryMiB, s.MemoryMiB))
			}
			if len(params) == 1 {
				continue
			}
			c.Op, c.Precondition = OpResize, o.Version
			c.Requests = []protocol.Approved{{Method: setConfig.Method, Path: setConfig.Path, Params: params}}
		}
		out.Changes = append(out.Changes, c)
	}
	return out, nil
}

// Apply implements sdk.Provider.
func (p Provider) Apply(ctx context.Context, h *sdk.Host, in protocol.ApplyParams) (protocol.ApplyResult, error) {
	c := in.Change
	node := c.IDs["node"]
	vmid, err := strconv.ParseInt(c.IDs["vmid"], 10, 64)
	if err != nil || len(c.Requests) != 1 {
		return protocol.ApplyResult{}, errors.New("a change names one VM and sends one request")
	}
	a := c.Requests[0]
	switch c.Op {
	case OpCreate:
		vms, err := list(ctx, h)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		if vm, ok := vms[vmid]; ok {
			cfg, err := config(ctx, h, vm.Node, vmid)
			if err != nil {
				return protocol.ApplyResult{}, err
			}
			if cfg.Description == Marker && vm.Name == a.Params["name"][0] {
				return protocol.ApplyResult{Message: "the VM exists already (an earlier apply created it)"}, nil
			}
			return protocol.ApplyResult{}, fmt.Errorf("VM %d exists and is not this provider's", vmid)
		}
		var upid string
		if err := call(ctx, h, protocol.HostRequest{Method: "POST", Path: "/nodes/" + node + "/qemu", Form: a.Params}, &upid); err != nil {
			return protocol.ApplyResult{}, err
		}
		if err := p.wait(ctx, h, node, upid); err != nil {
			return protocol.ApplyResult{}, err
		}
		return protocol.ApplyResult{Message: fmt.Sprintf("created VM %d", vmid)}, nil
	case OpResize:
		var ignored any
		if err := call(ctx, h, protocol.HostRequest{Method: "PUT", Path: fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), Form: a.Params}, &ignored); err != nil {
			return protocol.ApplyResult{}, err
		}
		return protocol.ApplyResult{Message: fmt.Sprintf("changed VM %d", vmid)}, nil
	}
	return protocol.ApplyResult{}, fmt.Errorf("unknown operation %s", c.Op)
}

// wait reads a task's status until it ends, and fails unless it ended OK.
func (p Provider) wait(ctx context.Context, h *sdk.Host, node, upid string) error {
	poll := p.Poll
	if poll == 0 {
		poll = time.Second
	}
	for range 300 {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: "/nodes/" + node + "/tasks/" + upid + "/status"}, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("task %s ended: %s", upid, st.ExitStatus)
			}
			return nil
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("task %s did not end", upid)
}

// Lookup implements sdk.Provider.
func (p Provider) Lookup(ctx context.Context, h *sdk.Host, in protocol.LookupParams) (protocol.LookupResult, error) {
	c := in.Change
	vmid, _ := strconv.ParseInt(c.IDs["vmid"], 10, 64)
	vms, err := list(ctx, h)
	if err != nil {
		return protocol.LookupResult{}, err
	}
	vm, ok := vms[vmid]
	if !ok {
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	cfg, err := config(ctx, h, vm.Node, vmid)
	if err != nil {
		return protocol.LookupResult{}, err
	}
	if cfg.Description != Marker {
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	if c.Op == OpCreate {
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	}
	want, done := 0, 0
	for k, v := range c.Requests[0].Params {
		switch k {
		case "cores":
			want++
			if strconv.FormatInt(cfg.Cores, 10) == v[0] {
				done++
			}
		case "memory":
			want++
			if strconv.FormatInt(cfg.Memory, 10) == v[0] {
				done++
			}
		}
	}
	switch {
	case done == want:
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	case done == 0:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupPartly}, nil
}

func spec(v jsontext.Value) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(v, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	switch {
	case s.Node == "" || s.Pool == "" || s.Storage == "":
		return s, errors.New("node, pool and storage are required")
	case s.VMID < 100 || s.VMID > 999999999:
		return s, fmt.Errorf("vmid %d is not a Proxmox VMID", s.VMID)
	case s.Cores < 1 || s.MemoryMiB < 16 || s.DiskGiB < 1:
		return s, errors.New("cores, memoryMiB (16 or more) and diskGiB must be positive")
	}
	return s, nil
}

type vmConfig struct {
	Digest      string `json:"digest"`
	Description string `json:"description"`
	Cores       int64  `json:"cores"`
	Memory      int64  `json:"memory"`
}

// UnmarshalJSON reads cores and memory as numbers or strings (Proxmox writes either).
func (c *vmConfig) UnmarshalJSON(b []byte) error {
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	num := func(v jsontext.Value) int64 {
		s := strings.Trim(string(v), `"`)
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	json.Unmarshal(raw["digest"], &c.Digest)
	json.Unmarshal(raw["description"], &c.Description)
	c.Description = strings.TrimSpace(c.Description)
	c.Cores, c.Memory = num(raw["cores"]), num(raw["memory"])
	if c.Cores == 0 {
		c.Cores = 1 // the Proxmox default
	}
	return nil
}

func config(ctx context.Context, h *sdk.Host, node string, vmid int64) (vmConfig, error) {
	var c vmConfig
	err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)}, &c)
	return c, err
}

func list(ctx context.Context, h *sdk.Host) (map[int64]resource, error) {
	var rs []resource
	if err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: "/cluster/resources", Query: map[string][]string{"type": {"vm"}}}, &rs); err != nil {
		return nil, err
	}
	out := map[int64]resource{}
	for _, r := range rs {
		if r.Type == "qemu" {
			out[r.VMID] = r
		}
	}
	return out, nil
}

// call sends a request and decodes Proxmox's {"data": ...}.
func call(ctx context.Context, h *sdk.Host, r protocol.HostRequest, out any) error {
	resp, err := h.Request(ctx, r)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("%s %s: status %d: %.300s", r.Method, r.Path, resp.Status, resp.Body)
	}
	var env struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &env); err != nil {
		return fmt.Errorf("%s %s: %w", r.Method, r.Path, err)
	}
	return json.Unmarshal(env.Data, out)
}
