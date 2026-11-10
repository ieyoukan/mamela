// Package pve is the Proxmox VE provider: it observes and creates VMs inside one lab boundary.
//
// VMs are created without a template. The provider uploads a pinned cloud image (see Catalog) to
// an image storage once, as content type "import", and creates each VM with
// `scsi0: <disk storage>:0,import-from=<image volume>`, a Proxmox cloud-init drive, a disk grown to
// the declared size, and an ownership marker. The operating token needs no privilege beyond the
// lab pool, its storages and its bridge: in particular not Sys.AccessNetwork (download-url) and no
// template to clone.
//
// Safety:
//   - The VMID of each VM is reserved in Mamela's database before anything is created, and never
//     changes, so a retried create targets the same VMID and cannot make a second VM.
//   - A VM is managed only if it is in the pool, in the VMID range, tagged, carries the marker and
//     holds the VMID reserved for its name. Anything else is Foreign and never changed.
//   - Only create-vm and grow-vm-disk are implemented. There is no delete, shrink, replace or
//     stop: the Proxmox client cannot even send DELETE.
//   - A write whose answer is lost is reported as reconcile.ErrUnknownOutcome; the next run
//     observes before doing anything, and a half-finished create is resumed, never repeated.
package pve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/proxmox"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
)

// API is the part of the Proxmox client the provider uses (the operating token).
type API interface {
	Resources(ctx context.Context) ([]proxmox.Resource, error)
	VMIDFree(ctx context.Context, vmid int) (bool, error)
	VMConfig(ctx context.Context, node string, vmid int) (proxmox.VMConfig, error)
	VMStatus(ctx context.Context, node string, vmid int) (proxmox.VMStatus, error)
	StorageContent(ctx context.Context, node, storage, content string) ([]proxmox.Volume, error)
	TaskStatus(ctx context.Context, node, upid string) (proxmox.Task, error)
	Tasks(ctx context.Context, node string, vmid int, typ string) ([]proxmox.TaskEntry, error)
	Upload(ctx context.Context, node, storage, content, filename, sha256hex string, file io.Reader, size int64) (string, error)
	CreateVM(ctx context.Context, node string, params url.Values) (string, error)
	GrowDisk(ctx context.Context, node string, vmid int, disk, size string) (string, error)
	StartVM(ctx context.Context, node string, vmid int) (string, error)
	SetMarker(ctx context.Context, node string, vmid int, description, tags, digest string) error
	SetSize(ctx context.Context, node string, vmid int, cores, memoryMiB int64, digest string) error
	RebootVM(ctx context.Context, node string, vmid int) (string, error)
}

// VMIDs reserves VMIDs by VM name (implemented by store.Store).
type VMIDs interface {
	VMID(ctx context.Context, name string) (int, bool, error)
	AllocateVMID(ctx context.Context, name string, min, max int, free func(context.Context, int) (bool, error)) (int, error)
}

// ImageFile is an opened, verified image.
type ImageFile interface {
	io.Reader
	io.Closer
}

// Seeds makes the cloud-init seed images that install mamelet (implemented by seed.Maker).
type Seeds interface {
	// Reusable names a seed already made for the VM, with this network, that can still be used.
	Reusable(ctx context.Context, node string, vmid int, network graph.Network) (string, bool, error)
	// New makes a seed with a fresh one-time enrollment token and returns its file name and image.
	New(ctx context.Context, node string, vmid int, network graph.Network) (string, []byte, error)
}

// Images opens verified image files (implemented by ImageCache).
type Images interface {
	Open(ctx context.Context, img Image) (ImageFile, error)
}

// Config is the lab boundary and placement the provider works in.
type Config struct {
	Node             string
	Pool             string
	Tag              string
	VMIDMin, VMIDMax int
	DiskStorage      string // where VM disks are allocated (content "images")
	ImageStorage     string // where cloud images are uploaded (content "import")
	Bridge           string

	MaxVMs       int
	MaxCPU       int64
	MaxMemoryMiB int64
	MaxDiskGiB   int64
	// Networks: DHCP allowed, and the range of static addresses (invalid when there is none).
	AllowDHCP               bool
	StaticFirst, StaticLast netip.Addr

	// Cloud-init for new VMs: the login user and its SSH public keys (one per line; may be empty).
	User           string
	SSHPublicKeys  string
	Catalog        map[string]Image // nil: the built-in Catalog
	PollInterval   time.Duration    // default 2s
	TaskTimeout    time.Duration    // default 2h (importing a disk can take long)
	UploadDeadline time.Duration    // default 60m
}

// Provider implements reconcile.Provider for VMs on one Proxmox node.
type Provider struct {
	Config Config
	API    API
	VMIDs  VMIDs
	Images Images
	// Seeds, when set, makes each new VM's cloud-init seed (an ISO with mamelet and its one-time
	// token, attached as a CD-ROM) instead of Proxmox's cloud-init drive.
	Seeds Seeds
	Clock func() time.Time
	Sleep func(context.Context, time.Duration) error // nil: a real sleep
}

var _ reconcile.Provider = (*Provider)(nil)

// lab is one VM inside the boundary, as observed.
type lab struct {
	vmid   int
	name   string // the Proxmox name
	marker marker
	hasMk  bool
	tagged bool
	owned  bool // marker, in range, reserved for its name, and tagged unless still creating
	config proxmox.VMConfig
	status string // running, stopped (from /cluster/resources)
	uptime int64  // seconds
}

// Observe reports the VMs the operating token can see. Packages and services are not observed
// here (that is the agent's job), so they are absent from the snapshot.
func (p *Provider) Observe(ctx context.Context) (state.Snapshot, error) {
	ctx = proxmox.Restrict(proxmox.ReadOnly(ctx, "observe"), "observe", ObserveCalls)
	now := p.Clock().UTC()
	snap := state.Empty(now)
	vms, err := p.scan(ctx)
	if err != nil {
		return state.Snapshot{}, err
	}
	for _, v := range vms {
		if v.owned {
			if v.marker.State != stateReady {
				// Still being created: reported as not existing, so the next create resumes it.
				continue
			}
			snap.VMs[v.marker.Name] = p.observed(v, true, now)
			continue
		}
		name := v.name
		if name == "" {
			name = "vmid-" + strconv.Itoa(v.vmid)
		}
		if _, dup := snap.VMs[name]; !dup {
			snap.VMs[name] = p.observed(v, false, now)
		}
	}
	return snap, nil
}

func (p *Provider) observed(v lab, managed bool, now time.Time) state.VM {
	cores := v.config.Int("cores")
	if cores == 0 {
		cores = 1
	}
	vm := state.VM{Managed: managed, CPU: cores, MemoryMiB: memoryMiB(v.config.String("memory")), ObservedAt: now,
		Stopped: v.status != "running"}
	if !vm.Stopped {
		vm.UptimeSeconds = v.uptime
	}
	if size, ok := diskBytes(v.config.String("scsi0")); ok {
		vm.DiskGiB = size >> 30
	}
	if v.hasMk {
		vm.Image, vm.Network = v.marker.Image, v.marker.Network
	}
	return vm
}

// scan lists the qemu guests the token can see and reads the configuration of those in the pool.
func (p *Provider) scan(ctx context.Context) ([]lab, error) {
	res, err := p.API.Resources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list guests: %w", err)
	}
	var out []lab
	for _, r := range res {
		if r.Type == "qemu" && r.Pool == "" && p.inRange(r.VMID) {
			// Proxmox shows the pool only to tokens with Pool.Audit on it. Without it, Mamela's own
			// VMs would look like someone else's and the plan would never settle.
			return nil, fmt.Errorf("guest %d is visible but Proxmox does not say which pool it is in: the operating "+
				"token needs Pool.Audit on /pool/%s (run the bootstrap script again)", r.VMID, p.Config.Pool)
		}
		if r.Type != "qemu" || r.Node != p.Config.Node || r.Pool != p.Config.Pool {
			continue
		}
		cfg, err := p.API.VMConfig(ctx, r.Node, r.VMID)
		if err != nil {
			return nil, fmt.Errorf("read the configuration of VM %d: %w", r.VMID, err)
		}
		v := lab{vmid: r.VMID, name: r.Name, config: cfg, status: r.Status, uptime: r.Uptime}
		v.marker, v.hasMk = parseMarker(cfg.String("description"))
		v.tagged = p.tagged(cfg.String("tags"))
		// A VM being created has no tag yet: Proxmox refuses tags from a pool-scoped token until
		// the VM is in the pool, so finish sets the tag together with the ready marker. A ready VM
		// without the tag has been changed by someone else and is not Mamela's.
		if v.hasMk && p.inRange(r.VMID) && (v.tagged || v.marker.State == stateCreating) {
			reserved, ok, err := p.VMIDs.VMID(ctx, v.marker.Name)
			if err != nil {
				return nil, err
			}
			v.owned = ok && reserved == r.VMID
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b lab) int { return a.vmid - b.vmid })
	return out, nil
}

func (p *Provider) inRange(vmid int) bool {
	return vmid >= p.Config.VMIDMin && vmid <= p.Config.VMIDMax
}

func (p *Provider) tagged(tags string) bool {
	return slices.Contains(strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), p.Config.Tag)
}

// Apply performs one step. Only create-vm and grow-vm-disk exist; anything else is refused before
// any request is sent.
func (p *Provider) Apply(ctx context.Context, step reconcile.Step) error {
	r := step.Resource
	if r.Kind != graph.KindVirtualMachine || r.VirtualMachine == nil {
		return fmt.Errorf("the Proxmox provider does not handle %s (%s)", r.Kind, step.Op)
	}
	spec := *r.VirtualMachine
	calls, ok := OperationCalls(step.Op, p.Seeds != nil)
	if !ok {
		return fmt.Errorf("the Proxmox provider does not implement %s", step.Op)
	}
	// The operation sends only the calls its manifest declares, changes only the VM it is bound
	// to (bindVM), and only with the approved values.
	ctx = proxmox.RestrictOperation(ctx, string(step.Op), append(slices.Clone(ObserveCalls), calls...))
	ctx = proxmox.Bind(ctx, map[string]string{"node": p.Config.Node, "storage": p.Config.ImageStorage})
	ctx = p.approvedValues(ctx, r.Name, spec)
	img, err := p.checkSpec(r.Name, spec)
	if err != nil {
		return err
	}
	switch step.Op {
	case policy.OpCreateVM:
		err = p.create(ctx, r.Name, spec, img)
	case policy.OpStartVM:
		err = p.start(ctx, r.Name)
	case policy.OpGrowVMDisk:
		err = p.grow(ctx, r.Name, spec)
	case policy.OpResizeVMCPU, policy.OpResizeVMMemory:
		err = p.resize(ctx, r.Name, spec)
	default:
		return fmt.Errorf("the Proxmox provider does not implement %s", step.Op)
	}
	// A failure after a change took effect is not a clean failure: the operation is partly done.
	// The reconciler then stops and the next run observes before doing anything. (A refusal before
	// sending, ErrNotSent, is only about that one request.)
	if changed := proxmox.Changed(ctx); err != nil && len(changed) > 0 && !errors.Is(err, reconcile.ErrUnknownOutcome) {
		return fmt.Errorf("%v (already done: %s): %w", err, strings.Join(changed, ", "), reconcile.ErrPartlyDone)
	}
	return err
}

// approvedValues pins the values that carry the approved intent on the requests that send them.
func (p *Provider) approvedValues(ctx context.Context, name string, spec graph.VirtualMachineSpec) context.Context {
	cores, memory := strconv.FormatInt(spec.CPU, 10), strconv.FormatInt(spec.MemoryMiB, 10)
	ctx = proxmox.Values(ctx, "POST", "/nodes/{node}/qemu", url.Values{
		"name": {name}, "cores": {cores}, "memory": {memory}, "pool": {p.Config.Pool}})
	ctx = proxmox.Values(ctx, "PUT", "/nodes/{node}/qemu/{vmid}/config", url.Values{
		"cores": {cores}, "memory": {memory}, "tags": {p.Config.Tag}})
	return proxmox.Values(ctx, "PUT", "/nodes/{node}/qemu/{vmid}/resize", url.Values{
		"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", spec.DiskGiB)}})
}

// bindVM binds the operation to the VM it acts on: from now on it changes no other VMID.
func bindVM(ctx context.Context, vmid int) context.Context {
	return proxmox.Bind(ctx, map[string]string{"vmid": strconv.Itoa(vmid)})
}

func (p *Provider) catalog() map[string]Image {
	if p.Config.Catalog != nil {
		return p.Config.Catalog
	}
	return Catalog
}

// checkSpec enforces the lab budget again, independently of the plan.
func (p *Provider) checkSpec(name string, s graph.VirtualMachineSpec) (Image, error) {
	img, ok := p.catalog()[s.Image]
	if !ok {
		return Image{}, fmt.Errorf("VM %s: image %q is not in the catalog (%s)", name, s.Image, strings.Join(sortedKeys(p.catalog()), ", "))
	}
	c := p.Config
	switch {
	case s.CPU < 1 || s.CPU > c.MaxCPU:
		return img, fmt.Errorf("VM %s: %d vCPU is outside the lab budget (1..%d)", name, s.CPU, c.MaxCPU)
	case s.MemoryMiB < 256 || s.MemoryMiB > c.MaxMemoryMiB:
		return img, fmt.Errorf("VM %s: %d MiB of memory is outside the lab budget (256..%d)", name, s.MemoryMiB, c.MaxMemoryMiB)
	case s.DiskGiB > c.MaxDiskGiB:
		return img, fmt.Errorf("VM %s: a %d GiB disk is over the lab budget (%d GiB)", name, s.DiskGiB, c.MaxDiskGiB)
	case s.DiskGiB < img.MinDiskGiB:
		return img, fmt.Errorf("VM %s: image %s needs a disk of at least %d GiB, got %d", name, s.Image, img.MinDiskGiB, s.DiskGiB)
	}
	if err := s.Network.Allowed(c.AllowDHCP, c.StaticFirst, c.StaticLast); err != nil {
		return img, fmt.Errorf("VM %s: %w", name, err)
	}
	return img, nil
}

// create makes the VM, or resumes a create that an earlier run started.
func (p *Provider) create(ctx context.Context, name string, spec graph.VirtualMachineSpec, img Image) error {
	existing, err := p.scan(ctx)
	if err != nil {
		return err
	}
	ours := 0
	for _, v := range existing {
		if v.hasMk && v.marker.Name == name {
			if !v.owned {
				return fmt.Errorf("VM %s: guest %d in pool %s carries Mamela's marker for %s, but this database has no "+
					"reservation of VMID %d for it (a different or lost database?); Mamela will not create another one",
					name, v.vmid, p.Config.Pool, name, v.vmid)
			}
			return p.finish(bindVM(ctx, v.vmid), v.vmid, name, spec) // resume
		}
		if v.owned {
			ours++
		}
	}
	if ours >= p.Config.MaxVMs {
		return fmt.Errorf("VM %s: the lab already has %d VM(s), the budget is %d", name, ours, p.Config.MaxVMs)
	}

	vmid, err := p.VMIDs.AllocateVMID(ctx, name, p.Config.VMIDMin, p.Config.VMIDMax, p.API.VMIDFree)
	if err != nil {
		return fmt.Errorf("VM %s: reserve a VMID: %w", name, err)
	}
	ctx = bindVM(ctx, vmid)
	free, err := p.API.VMIDFree(ctx, vmid)
	if err != nil {
		return fmt.Errorf("VM %s: check VMID %d: %w", name, vmid, err)
	}
	if !free {
		// Taken, but not visible as our VM in the pool: a create of ours that has not ended yet
		// (Proxmox adds the VM to the pool at the end of the task), or someone else's guest.
		return p.resume(ctx, vmid, name, spec)
	}

	volume, err := p.ensureImage(ctx, spec.Image, img)
	if err != nil {
		return fmt.Errorf("VM %s: %w", name, err)
	}
	seedVol := ""
	if p.Seeds != nil {
		if seedVol, err = p.ensureSeed(ctx, name, vmid, spec.Network); err != nil {
			return fmt.Errorf("VM %s: %w", name, err)
		}
	}
	params := p.Config.createParams(name, spec, vmid, volume, seedVol)
	upid, err := p.API.CreateVM(ctx, p.Config.Node, params)
	if err != nil {
		if !proxmox.Uncertain(err) {
			return writeErr(fmt.Sprintf("create VM %s (VMID %d)", name, vmid), err)
		}
		// The answer was lost: the create may or may not have started. Proxmox knows.
		t, ok, lerr := p.createTask(ctx, vmid)
		if lerr != nil || !ok {
			return writeErr(fmt.Sprintf("create VM %s (VMID %d)", name, vmid), err)
		}
		upid = t.UPID
	}
	return p.awaitCreate(ctx, vmid, name, spec, upid)
}

// resume continues a create whose VMID is in use but whose VM is not visible in the pool yet. Mamela
// never picks another VMID for the name: that is how duplicates are made. It asks Proxmox for the
// token's own create task for the VMID instead of guessing.
func (p *Provider) resume(ctx context.Context, vmid int, name string, spec graph.VirtualMachineSpec) error {
	t, ok, err := p.createTask(ctx, vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("VM %s: list the create tasks of VMID %d", name, vmid), err)
	}
	if !ok || (!t.Running() && t.Status != "OK" && !strings.HasPrefix(t.Status, "WARNINGS")) {
		last := "no create task of this token"
		if ok {
			last = "the last create task of this token failed: " + t.Status
		}
		return fmt.Errorf("VM %s: VMID %d, reserved for it, is in use by a guest outside pool %s that Mamela did not "+
			"create (%s). Remove that guest from VMID %d or release the reservation by hand; Mamela will not use "+
			"another VMID for %s", name, vmid, p.Config.Pool, last, vmid, name)
	}
	return p.awaitCreate(ctx, vmid, name, spec, t.UPID)
}

// createTask is the newest create task of this token for vmid, running or not.
func (p *Provider) createTask(ctx context.Context, vmid int) (proxmox.TaskEntry, bool, error) {
	tasks, err := p.API.Tasks(ctx, p.Config.Node, vmid, "qmcreate")
	if err != nil || len(tasks) == 0 {
		return proxmox.TaskEntry{}, false, err
	}
	return tasks[0], true, nil
}

// awaitCreate waits for the create task to end, then finishes the VM. A failed task is a definite
// failure with Proxmox's reason (Proxmox removes the VM again), and the next attempt uses the same
// VMID. A task still running at the deadline is picked up again by the next apply.
func (p *Provider) awaitCreate(ctx context.Context, vmid int, name string, spec graph.VirtualMachineSpec, upid string) error {
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return fmt.Errorf("create VM %s (VMID %d): %w", name, vmid, err)
	}
	return p.finish(ctx, vmid, name, spec)
}

// finish brings a created VM to ready: full disk size, started, marker set. Each step checks the
// current state first, so finishing again after an interruption is safe.
func (p *Provider) finish(ctx context.Context, vmid int, name string, spec graph.VirtualMachineSpec) error {
	st, err := p.API.VMStatus(ctx, p.Config.Node, vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("read the status of VM %s", name), err)
	}
	if st.Lock != "" {
		return fmt.Errorf("VM %s (VMID %d) is locked (%s): an earlier operation is still running: %w", name, vmid, st.Lock, reconcile.ErrUnknownOutcome)
	}
	if err := p.growTo(ctx, vmid, name, spec.DiskGiB); err != nil {
		return err
	}
	// The VM stays stopped: start-vm starts it after every VM of the run is created, so that its
	// first boot does not compete with another VM's disk import for the storage.
	cfg, err := p.API.VMConfig(ctx, p.Config.Node, vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("read the configuration of VM %s", name), err)
	}
	m, ok := parseMarker(cfg.String("description"))
	if !ok || m.Name != name {
		return fmt.Errorf("VM %s (VMID %d) lost its ownership marker; Mamela does not touch it further", name, vmid)
	}
	if m.State != stateReady {
		m.State = stateReady
		if err := p.API.SetMarker(ctx, p.Config.Node, vmid, m.String(), p.Config.Tag, cfg.String("digest")); err != nil {
			return writeErr(fmt.Sprintf("mark VM %s ready", name), err)
		}
	}
	return nil
}

// start starts a managed, ready VM that is not running.
func (p *Provider) start(ctx context.Context, name string) error {
	v, err := p.managed(ctx, name)
	if err != nil {
		return err
	}
	ctx = bindVM(ctx, v.vmid)
	st, err := p.API.VMStatus(ctx, p.Config.Node, v.vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("read the status of VM %s", name), err)
	}
	if st.Lock != "" {
		return fmt.Errorf("VM %s (VMID %d) is locked (%s): an earlier operation is still running: %w", name, v.vmid, st.Lock, reconcile.ErrUnknownOutcome)
	}
	if st.Status == "running" {
		return nil
	}
	upid, err := p.API.StartVM(ctx, p.Config.Node, v.vmid)
	if err != nil {
		return writeErr(fmt.Sprintf("start VM %s", name), err)
	}
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return fmt.Errorf("start VM %s: %w", name, err)
	}
	return nil
}

// grow extends the disk of a managed, ready VM.
func (p *Provider) grow(ctx context.Context, name string, spec graph.VirtualMachineSpec) error {
	v, err := p.managed(ctx, name)
	if err != nil {
		return err
	}
	return p.growTo(bindVM(ctx, v.vmid), v.vmid, name, spec.DiskGiB)
}

// managed finds the VM Mamela created for name (ready, owned, its reserved VMID).
func (p *Provider) managed(ctx context.Context, name string) (lab, error) {
	vmid, ok, err := p.VMIDs.VMID(ctx, name)
	if err != nil {
		return lab{}, err
	}
	if !ok {
		return lab{}, fmt.Errorf("VM %s has no VMID reserved by Mamela; it is not managed", name)
	}
	vms, err := p.scan(ctx)
	if err != nil {
		return lab{}, err
	}
	for _, v := range vms {
		if v.vmid == vmid && v.owned && v.marker.Name == name && v.marker.State == stateReady {
			return v, nil
		}
	}
	return lab{}, fmt.Errorf("VM %s (VMID %d) is not a managed VM in pool %s", name, vmid, p.Config.Pool)
}

// resize sets the declared vCPUs and memory in one change (whichever differ), and restarts a
// running VM so that it takes effect (Proxmox keeps CPU and memory changes pending until then;
// its reboot applies pending changes). Both resize operations call it: the second finds nothing
// left to do, so a VM is restarted once.
func (p *Provider) resize(ctx context.Context, name string, spec graph.VirtualMachineSpec) error {
	v, err := p.managed(ctx, name)
	if err != nil {
		return err
	}
	ctx = bindVM(ctx, v.vmid)
	cores := v.config.Int("cores")
	if cores == 0 {
		cores = 1
	}
	if cores == spec.CPU && memoryMiB(v.config.String("memory")) == spec.MemoryMiB {
		return nil
	}
	if err := p.API.SetSize(ctx, p.Config.Node, v.vmid, spec.CPU, spec.MemoryMiB, v.config.String("digest")); err != nil {
		return writeErr(fmt.Sprintf("resize VM %s", name), err)
	}
	st, err := p.API.VMStatus(ctx, p.Config.Node, v.vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("read the status of VM %s", name), err)
	}
	if st.Status == "running" {
		upid, err := p.API.RebootVM(ctx, p.Config.Node, v.vmid)
		if err != nil {
			return writeErr(fmt.Sprintf("restart VM %s to apply the new size", name), err)
		}
		if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
			return fmt.Errorf("restart VM %s to apply the new size: %w", name, err)
		}
	}
	return nil
}

func (p *Provider) growTo(ctx context.Context, vmid int, name string, gib int64) error {
	cfg, err := p.API.VMConfig(ctx, p.Config.Node, vmid)
	if err != nil {
		return unknownErr(fmt.Sprintf("read the configuration of VM %s", name), err)
	}
	size, ok := diskBytes(cfg.String("scsi0"))
	if !ok {
		return fmt.Errorf("VM %s (VMID %d) has no readable scsi0 disk", name, vmid)
	}
	if size >= gib<<30 {
		return nil // never shrink; equal is done
	}
	upid, err := p.API.GrowDisk(ctx, p.Config.Node, vmid, "scsi0", fmt.Sprintf("%dG", gib))
	if err != nil {
		return writeErr(fmt.Sprintf("grow the disk of VM %s to %d GiB", name, gib), err)
	}
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return fmt.Errorf("grow the disk of VM %s: %w", name, err)
	}
	return nil
}

// ensureImage uploads the image once and returns its volume ID ("<storage>:import/<file>").
// createParams is everything sent to create a VM. Every key must be in the permission catalog
// (proxmox.CreateParams); the operating token's grants are derived from these keys.
func (c Config) createParams(name string, spec graph.VirtualMachineSpec, vmid int, volume, seedVol string) url.Values {
	params := url.Values{
		"vmid": {strconv.Itoa(vmid)},
		"name": {name},
		"pool": {c.Pool},
		// No tags: Proxmox checks them before the VM is in the pool. finish adds the tag.
		"description": {marker{Name: name, Image: spec.Image, Network: spec.Network.String(), State: stateCreating}.String()},
		"ostype":      {"l26"},
		"cores":       {strconv.FormatInt(spec.CPU, 10)},
		"memory":      {strconv.FormatInt(spec.MemoryMiB, 10)},
		"scsihw":      {"virtio-scsi-single"},
		"scsi0":       {c.DiskStorage + ":0,import-from=" + volume + ",discard=on"},
		"boot":        {"order=scsi0"},
		"serial0":     {"socket"},
		"vga":         {"serial0"},
		"net0":        {"virtio,bridge=" + c.Bridge},
		"onboot":      {"0"},
	}
	if seedVol != "" {
		// Our own NoCloud seed replaces Proxmox's cloud-init drive (two would conflict): it holds
		// the user and keys too, plus mamelet. qemu-guest-agent (installed by the seed) reports the
		// VM's addresses to Proxmox.
		params.Set("ide2", seedVol+",media=cdrom")
		params.Set("agent", "enabled=1")
		return params
	}
	params.Set("ide2", c.DiskStorage+":cloudinit")
	if n := spec.Network; n.Mode == graph.NetworkStatic {
		params.Set("ipconfig0", "ip="+n.Address+",gw="+n.Gateway)
		params.Set("nameserver", strings.Join(n.DNS, " "))
	} else {
		params.Set("ipconfig0", "ip=dhcp")
	}
	params.Set("ciuser", c.User)
	if keys := strings.TrimSpace(c.SSHPublicKeys); keys != "" {
		// Proxmox expects the keys URL-encoded inside the (also encoded) form value.
		params.Set("sshkeys", strings.ReplaceAll(url.QueryEscape(keys+"\n"), "+", "%20"))
	}
	return params
}

// CreateParamNames are the parameters sent to create a VM, with or without a mamelet seed.
func CreateParamNames(withSeed bool) []string {
	c := Config{Pool: "p", DiskStorage: "d", Bridge: "b", User: "u", SSHPublicKeys: "ssh-ed25519 k"}
	seed := ""
	if withSeed {
		seed = "i:iso/s.iso"
	}
	names := map[string]bool{}
	for _, n := range []graph.Network{{Mode: graph.NetworkDHCP},
		{Mode: graph.NetworkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}} {
		for k := range c.createParams("n", graph.VirtualMachineSpec{CPU: 1, MemoryMiB: 1, Image: "x", Network: n}, 1, "v", seed) {
			names[k] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// ensureSeed returns the seed volume for the VM, uploading a new seed unless one made for it earlier
// (a retried create) is still on the storage and its token still good.
func (p *Provider) ensureSeed(ctx context.Context, name string, vmid int, network graph.Network) (string, error) {
	volid := func(file string) string { return p.Config.ImageStorage + ":iso/" + file }
	if file, ok, err := p.Seeds.Reusable(ctx, name, vmid, network); err != nil {
		return "", err
	} else if ok {
		vols, err := p.API.StorageContent(ctx, p.Config.Node, p.Config.ImageStorage, "iso")
		if err != nil {
			return "", fmt.Errorf("list ISOs on %s: %w", p.Config.ImageStorage, err)
		}
		if slices.ContainsFunc(vols, func(v proxmox.Volume) bool { return v.VolID == volid(file) }) {
			return volid(file), nil
		}
	}
	file, img, err := p.Seeds.New(ctx, name, vmid, network)
	if err != nil {
		return "", fmt.Errorf("make the cloud-init seed: %w", err)
	}
	sum := sha256.Sum256(img)
	upid, err := p.API.Upload(ctx, p.Config.Node, p.Config.ImageStorage, "iso", file, hex.EncodeToString(sum[:]), bytes.NewReader(img), int64(len(img)))
	if err != nil {
		return "", fmt.Errorf("upload the cloud-init seed to %s: %w", p.Config.ImageStorage, err)
	}
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return "", fmt.Errorf("upload the cloud-init seed: %w", err)
	}
	return volid(file), nil
}

func (p *Provider) ensureImage(ctx context.Context, name string, img Image) (string, error) {
	file := VolumeName(name, img)
	want := p.Config.ImageStorage + ":import/" + file
	has := func() (bool, error) {
		vols, err := p.API.StorageContent(ctx, p.Config.Node, p.Config.ImageStorage, "import")
		if err != nil {
			return false, fmt.Errorf("list images on %s: %w", p.Config.ImageStorage, err)
		}
		return slices.ContainsFunc(vols, func(v proxmox.Volume) bool { return v.VolID == want && v.Size == img.Size }), nil
	}
	if ok, err := has(); err != nil || ok {
		return want, err
	}
	f, err := p.Images.Open(ctx, img)
	if err != nil {
		return "", fmt.Errorf("image %s: %w", name, err)
	}
	defer f.Close()
	deadline := p.Config.UploadDeadline
	if deadline == 0 {
		deadline = 60 * time.Minute
	}
	uctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	upid, err := p.API.Upload(uctx, p.Config.Node, p.Config.ImageStorage, "import", file, img.SHA256, f, img.Size)
	if err != nil {
		// An upload that may have landed is harmless: the file name contains the checksum and is
		// checked before use. Report it as a plain failure so the next run looks again.
		return "", fmt.Errorf("upload image %s to %s: %w", name, p.Config.ImageStorage, err)
	}
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return "", fmt.Errorf("upload image %s: %w", name, err)
	}
	if ok, err := has(); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%s is not on %s after the upload", file, p.Config.ImageStorage)
		}
		return "", err
	}
	return want, nil
}

// wait polls a task until it ends. Reading its status may fail for a while (a busy node, a
// network blip) without changing anything, so a few failures in a row are tolerated. Only a task
// that is still running at the deadline, or whose status cannot be read, is an unknown outcome;
// the task keeps its ID, so the next run waits for the same task instead of starting another.
func (p *Provider) wait(ctx context.Context, upid string, timeout time.Duration) error {
	interval := p.Config.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	const maxReadFailures = 5
	start := p.Clock()
	failures := 0
	for {
		t, err := p.API.TaskStatus(ctx, p.Config.Node, upid)
		switch {
		case err != nil && proxmox.Uncertain(err) && failures < maxReadFailures:
			failures++
		case err != nil:
			return unknownErr("read task "+upid, err)
		case t.Status == "stopped" && t.Succeeded():
			return nil
		case t.Status == "stopped":
			if why := proxmox.ExplainPermission(t.ExitStatus); why != "" {
				return fmt.Errorf("Proxmox task failed: %s — %s", t.ExitStatus, why)
			}
			return fmt.Errorf("Proxmox task failed: %s", t.ExitStatus)
		default:
			failures = 0
		}
		if p.Clock().Sub(start) > timeout {
			return fmt.Errorf("task %s is still running after %s; apply again to keep waiting for the same task "+
				"(nothing is created twice): %w", upid, timeout, reconcile.ErrUnknownOutcome)
		}
		if err := p.sleep(ctx, interval); err != nil {
			return unknownErr("wait for task "+upid, err)
		}
	}
}

func (p *Provider) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *Provider) taskTimeout() time.Duration {
	if p.Config.TaskTimeout == 0 {
		return 2 * time.Hour
	}
	return p.Config.TaskTimeout
}

// writeErr classifies a failed write: lost answers are unknown outcomes, refusals are failures.
func writeErr(what string, err error) error {
	if proxmox.Uncertain(err) {
		return fmt.Errorf("%s: %v: %w", what, err, reconcile.ErrUnknownOutcome)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// unknownErr is for reads after a write: not knowing the state means not knowing the outcome.
func unknownErr(what string, err error) error {
	if errors.Is(err, reconcile.ErrUnknownOutcome) {
		return err
	}
	return fmt.Errorf("%s: %v: %w", what, err, reconcile.ErrUnknownOutcome)
}

// diskBytes reads size=<n>[KMGT] from a drive option.
func diskBytes(drive string) (int64, bool) {
	for _, part := range strings.Split(drive, ",") {
		v, ok := strings.CutPrefix(part, "size=")
		if !ok || v == "" {
			continue
		}
		mult := int64(1)
		switch v[len(v)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			v = v[:len(v)-1]
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return int64(n * float64(mult)), true
	}
	return 0, false
}

// memoryMiB reads the memory option ("1024" or "current=1024,...").
func memoryMiB(s string) int64 {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimPrefix(part, "current=")
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func sortedKeys(m map[string]Image) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
