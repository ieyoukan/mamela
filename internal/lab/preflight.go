package lab

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/proxmox"
)

// Severity of a finding.
type Severity string

const (
	OK    Severity = "ok"
	Warn  Severity = "warn"
	Block Severity = "BLOCK"
)

// Finding is one preflight or postflight result.
type Finding struct {
	Code     string
	Severity Severity
	Message  string
}

// Blocked reports whether any finding blocks the run.
func Blocked(fs []Finding) bool {
	return slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == Block })
}

// Guest is a VM or container as seen by the audit token.
type Guest struct {
	VMID         int    `json:"vmid"`
	Type         string `json:"type"` // qemu or lxc
	Node         string `json:"node"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Pool         string `json:"pool"`
	Tags         string `json:"tags"`
	Template     bool   `json:"template"`
	Uptime       int64  `json:"uptime"`
	ConfigDigest string `json:"configDigest"` // empty when the node is unreachable
}

// Facts is everything preflight decides from. Gathering is separate from checking, so the checks
// are pure and tested without a cluster.
type Facts struct {
	Quorate     bool
	NodesTotal  int
	NodesOnline int
	NodeOnline  bool // the lab node
	Guests      []Guest
	// The lab node's memory and the disk storage's usage.
	MemAvailable int64
	DiskTotal    int64
	DiskUsed     int64
	// The operating token's own view: its permissions and the guests it can see.
	LabPermissions   map[string]map[string]int
	LabVisibleGuests []Guest
	// Storages on the lab node that accept importable images and that the operating token can use.
	ImportStorages []string
}

// API is the read-only part of the Proxmox client that preflight needs.
type API interface {
	ClusterStatus(ctx context.Context) ([]proxmox.ClusterStatusEntry, error)
	Resources(ctx context.Context) ([]proxmox.Resource, error)
	NodeStatus(ctx context.Context, node string) (proxmox.NodeStatus, error)
	StorageStatus(ctx context.Context, node, storage string) (proxmox.StorageStatus, error)
	Permissions(ctx context.Context) (map[string]map[string]int, error)
	ConfigDigest(ctx context.Context, node, kind string, vmid int) (string, error)
	Storages(ctx context.Context, node, content string) ([]string, error)
}

// Gather collects the facts. audit is the read-only audit token; operating is the lab token.
func Gather(ctx context.Context, s *Site, audit, operating API) (*Facts, error) {
	f := &Facts{}
	status, err := audit.ClusterStatus(ctx)
	if err != nil {
		return nil, err
	}
	onlineNodes := map[string]bool{}
	for _, e := range status {
		switch e.Type {
		case "cluster":
			f.Quorate = e.Quorate == 1
			f.NodesTotal = e.Nodes
		case "node":
			if e.Online == 1 {
				f.NodesOnline++
				onlineNodes[e.Name] = true
			}
		}
	}
	if f.NodesTotal == 0 { // a single node without a cluster
		f.NodesTotal, f.Quorate = f.NodesOnline, true
	}
	f.NodeOnline = onlineNodes[s.Proxmox.Node]

	resources, err := audit.Resources(ctx)
	if err != nil {
		return nil, err
	}
	f.Guests = guestsOf(resources)
	for i, g := range f.Guests {
		if !onlineNodes[g.Node] {
			continue // the config lives on an unreachable node; status alone is compared
		}
		d, err := audit.ConfigDigest(ctx, g.Node, g.Type, g.VMID)
		if err != nil {
			return nil, err
		}
		f.Guests[i].ConfigDigest = d
	}

	if f.NodeOnline {
		ns, err := audit.NodeStatus(ctx, s.Proxmox.Node)
		if err != nil {
			return nil, err
		}
		f.MemAvailable = ns.Memory.Available
		ss, err := audit.StorageStatus(ctx, s.Proxmox.Node, s.Placement.DiskStorage)
		if err != nil {
			return nil, err
		}
		f.DiskTotal, f.DiskUsed = ss.Total, ss.Used
	}

	if f.LabPermissions, err = operating.Permissions(ctx); err != nil {
		return nil, fmt.Errorf("operating token: %w", err)
	}
	visible, err := operating.Resources(ctx)
	if err != nil {
		return nil, fmt.Errorf("operating token: %w", err)
	}
	f.LabVisibleGuests = guestsOf(visible)
	if f.NodeOnline {
		if f.ImportStorages, err = operating.Storages(ctx, s.Proxmox.Node, "import"); err != nil {
			return nil, fmt.Errorf("operating token: %w", err)
		}
	}
	return f, nil
}

func guestsOf(rs []proxmox.Resource) []Guest {
	var out []Guest
	for _, r := range rs {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		out = append(out, Guest{VMID: r.VMID, Type: r.Type, Node: r.Node, Name: r.Name, Status: r.Status,
			Pool: r.Pool, Tags: r.Tags, Template: r.Template == 1, Uptime: r.Uptime})
	}
	slices.SortFunc(out, func(a, b Guest) int { return a.VMID - b.VMID })
	return out
}

func (s *Site) inRange(vmid int) bool {
	return vmid >= s.Boundary.VMIDMin && vmid <= s.Boundary.VMIDMax
}

func hasTag(tags, tag string) bool {
	return slices.Contains(strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), tag)
}

// IsLab reports whether a guest is inside the lab boundary: in the pool and in the VMID range. The
// tag is not required here: Proxmox lets a pool-scoped token tag a VM only after its create task
// has put it in the pool, so Mamela tags a VM when it finishes it, and a half-finished one is still
// a lab guest. Whether Mamela manages a lab guest (marker, reservation, tag) is the provider's call.
func (s *Site) IsLab(g Guest) bool {
	return g.Pool == s.Boundary.Pool && s.inRange(g.VMID)
}

// Check evaluates the facts for creating one more lab VM of the maximum budgeted size.
func Check(s *Site, f *Facts) []Finding { return CheckFor(s, f, 1) }

// CheckFor evaluates the facts for creating newVMs more lab VMs of the maximum budgeted size
// (0 when a run only changes or finishes existing ones).
func CheckFor(s *Site, f *Facts, newVMs int) []Finding {
	var out []Finding
	add := func(code string, sev Severity, format string, args ...any) {
		out = append(out, Finding{Code: code, Severity: sev, Message: fmt.Sprintf(format, args...)})
	}

	// Cluster health.
	if f.Quorate {
		add("LAB-0001", OK, "the cluster is quorate")
	} else {
		add("LAB-0001", Block, "the cluster is not quorate; nothing may be changed")
	}
	if need := f.NodesTotal/2 + 1; f.NodesOnline-need <= 0 && f.NodesTotal > 1 {
		add("LAB-0002", Warn, "%d of %d nodes are online and %d are needed for quorum: losing one more node loses quorum", f.NodesOnline, f.NodesTotal, need)
	} else {
		add("LAB-0002", OK, "%d of %d nodes online", f.NodesOnline, f.NodesTotal)
	}
	if f.NodeOnline {
		add("LAB-0003", OK, "lab node %s is online", s.Proxmox.Node)
	} else {
		add("LAB-0003", Block, "lab node %s is not online", s.Proxmox.Node)
	}

	// The operating token's reach.
	if excess := s.ExcessPermissions(f.LabPermissions); len(excess) > 0 {
		add("LAB-0101", Block, "the operating token has permissions that no API call Mamela makes needs: %s; "+
			"run the bootstrap script again (it removes them), and see `mamela lab permissions`", strings.Join(excess, ", "))
	} else {
		add("LAB-0101", OK, "the operating token's permissions are within the lab boundary")
	}
	if missing := s.MissingPermissions(f.LabPermissions); len(missing) > 0 {
		add("LAB-0104", Block, "the operating token lacks %s; review and run the bootstrap script again (mamela lab bootstrap)", strings.Join(missing, "; "))
	} else {
		add("LAB-0104", OK, "the operating token has every permission the lab needs")
	}
	// Pool membership comes from the audit token's view: the operating token may not be shown it.
	poolOf := map[int]string{}
	for _, g := range f.Guests {
		poolOf[g.VMID] = g.Pool
	}
	var seen []string
	for _, g := range f.LabVisibleGuests {
		if poolOf[g.VMID] != s.Boundary.Pool {
			seen = append(seen, fmt.Sprintf("%d (%s)", g.VMID, g.Name))
		}
	}
	if len(seen) > 0 {
		add("LAB-0102", Block, "the operating token can see guests outside the lab pool: %s", strings.Join(seen, ", "))
	} else {
		add("LAB-0102", OK, "the operating token sees only the lab pool")
	}

	// Shared resources on the lab node, for one more VM of the maximum size.
	vmMem := int64(newVMs) * s.Budget.MaxMemoryMiBPerVM << 20
	leftMiB := (f.MemAvailable - vmMem) >> 20
	if leftMiB < s.Safety.MinHostMemAvailableMiB {
		add("LAB-0201", Block, "only %d MiB would remain available on %s after a %d MiB VM (minimum %d MiB)",
			leftMiB, s.Proxmox.Node, s.Budget.MaxMemoryMiBPerVM, s.Safety.MinHostMemAvailableMiB)
	} else {
		add("LAB-0201", OK, "%d MiB would remain available on %s after a %d MiB VM", leftMiB, s.Proxmox.Node, s.Budget.MaxMemoryMiBPerVM)
	}
	if f.DiskTotal <= 0 {
		add("LAB-0202", Block, "the size of storage %s is unknown", s.Placement.DiskStorage)
	} else {
		after := float64(f.DiskUsed+int64(newVMs)*s.Budget.MaxDiskGiBPerVM<<30) / float64(f.DiskTotal) * 100
		if after > float64(s.Safety.MaxThinpoolUsedPercent) {
			add("LAB-0202", Block, "storage %s would be %.1f%% used after a %d GiB disk (maximum %d%%)",
				s.Placement.DiskStorage, after, s.Budget.MaxDiskGiBPerVM, s.Safety.MaxThinpoolUsedPercent)
		} else {
			add("LAB-0202", OK, "storage %s would be %.1f%% used after a %d GiB disk", s.Placement.DiskStorage, after, s.Budget.MaxDiskGiBPerVM)
		}
	}

	if slices.Contains(f.ImportStorages, s.Placement.ImageStorage) {
		add("LAB-0203", OK, "storage %s accepts cloud images to import", s.Placement.ImageStorage)
	} else {
		add("LAB-0203", Block, "storage %s does not accept importable images (content type \"import\") for the operating token; run the bootstrap script again",
			s.Placement.ImageStorage)
	}

	// The boundary itself.
	labCount := 0
	for _, g := range f.Guests {
		inPool := g.Pool == s.Boundary.Pool
		switch {
		case s.inRange(g.VMID) && !s.IsLab(g):
			add("LAB-0301", Block, "guest %d (%s) is in the lab VMID range but not in pool %s (pool %q): someone else's guest, or a create still running", g.VMID, g.Name, s.Boundary.Pool, g.Pool)
		case inPool && !s.IsLab(g):
			add("LAB-0302", Block, "guest %d (%s) is in the lab pool but outside the VMID range", g.VMID, g.Name)
		case s.IsLab(g):
			labCount++
		}
	}
	if labCount+newVMs > s.Budget.MaxVMs {
		add("LAB-0303", Block, "the lab already has %d VM(s); %d more would exceed max_vms %d", labCount, newVMs, s.Budget.MaxVMs)
	} else {
		add("LAB-0303", OK, "the lab has %d VM(s); budget allows %d", labCount, s.Budget.MaxVMs)
	}
	return out
}

// Baseline is the state of every guest outside the lab, recorded before a live test.
type Baseline struct {
	Schema string  `json:"schema"`
	Guests []Guest `json:"guests"`
}

const baselineSchema = "mamela-lab-baseline/v1"

// NewBaseline records the guests outside the lab boundary.
func NewBaseline(s *Site, f *Facts) Baseline {
	b := Baseline{Schema: baselineSchema, Guests: []Guest{}}
	for _, g := range f.Guests {
		if !s.IsLab(g) {
			b.Guests = append(b.Guests, g)
		}
	}
	return b
}

// Encode is the baseline as JSON.
func (b Baseline) Encode() []byte {
	out, err := json.Marshal(b, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return out
}

// DecodeBaseline reads a baseline strictly.
func DecodeBaseline(data []byte) (Baseline, error) {
	var b Baseline
	if err := json.Unmarshal(data, &b, json.RejectUnknownMembers(true)); err != nil {
		return Baseline{}, err
	}
	if b.Schema != baselineSchema {
		return Baseline{}, fmt.Errorf("baseline schema %q is not %q", b.Schema, baselineSchema)
	}
	return b, nil
}

// Compare checks that every guest outside the lab is as it was in the baseline.
func Compare(s *Site, before Baseline, after *Facts) []Finding {
	var out []Finding
	add := func(code string, sev Severity, format string, args ...any) {
		out = append(out, Finding{Code: code, Severity: sev, Message: fmt.Sprintf(format, args...)})
	}
	now := map[int]Guest{}
	for _, g := range after.Guests {
		now[g.VMID] = g
	}
	changed := 0
	for _, b := range before.Guests {
		a, ok := now[b.VMID]
		switch {
		case !ok:
			add("LAB-0401", Block, "guest %d (%s) has disappeared", b.VMID, b.Name)
		case a.Status != b.Status:
			add("LAB-0402", Block, "guest %d (%s) changed status: %s -> %s", b.VMID, b.Name, b.Status, a.Status)
		case b.ConfigDigest != "" && a.ConfigDigest != "" && a.ConfigDigest != b.ConfigDigest:
			add("LAB-0403", Block, "guest %d (%s) configuration changed", b.VMID, b.Name)
		case b.Status == "running" && a.Uptime < b.Uptime:
			add("LAB-0404", Block, "guest %d (%s) restarted (uptime %ds -> %ds)", b.VMID, b.Name, b.Uptime, a.Uptime)
		default:
			continue
		}
		changed++
	}
	known := map[int]bool{}
	for _, b := range before.Guests {
		known[b.VMID] = true
	}
	for _, g := range after.Guests {
		switch {
		case known[g.VMID] || s.IsLab(g):
		case s.inRange(g.VMID):
			add("LAB-0405", Warn, "guest %d (%s) is in the lab VMID range but not in pool %s: a create that has not finished (Proxmox adds the VM to the pool at the end), or someone else's guest", g.VMID, g.Name, s.Boundary.Pool)
		default:
			add("LAB-0405", Warn, "a new guest %d (%s) appeared outside the lab (not created by Mamela?)", g.VMID, g.Name)
		}
	}
	if changed == 0 {
		add("LAB-0400", OK, "all %d guests outside the lab are unchanged", len(before.Guests))
	}
	return out
}
