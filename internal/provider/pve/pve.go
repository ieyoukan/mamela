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
	"context"
	"errors"
	"fmt"
	"io"
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
	Upload(ctx context.Context, node, storage, content, filename, sha256hex string, file io.Reader, size int64) (string, error)
	CreateVM(ctx context.Context, node string, params url.Values) (string, error)
	GrowDisk(ctx context.Context, node string, vmid int, disk, size string) (string, error)
	StartVM(ctx context.Context, node string, vmid int) (string, error)
	SetDescription(ctx context.Context, node string, vmid int, description, digest string) error
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

	// Cloud-init for new VMs: the login user and its SSH public keys (one per line; may be empty).
	User           string
	SSHPublicKeys  string
	Catalog        map[string]Image // nil: the built-in Catalog
	PollInterval   time.Duration    // default 2s
	TaskTimeout    time.Duration    // default 20m
	UploadDeadline time.Duration    // default 60m
}

// Provider implements reconcile.Provider for VMs on one Proxmox node.
type Provider struct {
	Config Config
	API    API
	VMIDs  VMIDs
	Images Images
	Clock  func() time.Time
	Sleep  func(context.Context, time.Duration) error // nil: a real sleep
}

var _ reconcile.Provider = (*Provider)(nil)

// lab is one VM inside the boundary, as observed.
type lab struct {
	vmid   int
	name   string // the Proxmox name
	marker marker
	hasMk  bool
	owned  bool // marker valid and the reservation for its name is this VMID
	config proxmox.VMConfig
}

// Observe reports the VMs the operating token can see. Packages and services are not observed
// here (that is the agent's job), so they are absent from the snapshot.
func (p *Provider) Observe(ctx context.Context) (state.Snapshot, error) {
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
	vm := state.VM{Managed: managed, CPU: cores, MemoryMiB: memoryMiB(v.config.String("memory")), ObservedAt: now}
	if size, ok := diskBytes(v.config.String("scsi0")); ok {
		vm.DiskGiB = size >> 30
	}
	if v.hasMk {
		vm.Image = v.marker.Image
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
		if r.Type != "qemu" || r.Node != p.Config.Node || r.Pool != p.Config.Pool {
			continue
		}
		cfg, err := p.API.VMConfig(ctx, r.Node, r.VMID)
		if err != nil {
			return nil, fmt.Errorf("read the configuration of VM %d: %w", r.VMID, err)
		}
		v := lab{vmid: r.VMID, name: r.Name, config: cfg}
		v.marker, v.hasMk = parseMarker(cfg.String("description"))
		if v.hasMk && p.inBoundary(r.VMID, cfg.String("tags")) {
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

func (p *Provider) inBoundary(vmid int, tags string) bool {
	return vmid >= p.Config.VMIDMin && vmid <= p.Config.VMIDMax &&
		slices.Contains(strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), p.Config.Tag)
}

// Apply performs one step. Only create-vm and grow-vm-disk exist; anything else is refused before
// any request is sent.
func (p *Provider) Apply(ctx context.Context, step reconcile.Step) error {
	r := step.Resource
	if r.Kind != graph.KindVirtualMachine || r.VirtualMachine == nil {
		return fmt.Errorf("the Proxmox provider does not handle %s (%s)", r.Kind, step.Op)
	}
	spec := *r.VirtualMachine
	img, err := p.checkSpec(r.Name, spec)
	if err != nil {
		return err
	}
	switch step.Op {
	case policy.OpCreateVM:
		return p.create(ctx, r.Name, spec, img)
	case policy.OpGrowVMDisk:
		return p.grow(ctx, r.Name, spec)
	default:
		return fmt.Errorf("the Proxmox provider does not implement %s", step.Op)
	}
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
			return p.finish(ctx, v.vmid, name, spec) // resume
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
	free, err := p.API.VMIDFree(ctx, vmid)
	if err != nil {
		return fmt.Errorf("VM %s: check VMID %d: %w", name, vmid, err)
	}
	if !free {
		// Taken, but not visible as our VM in the pool: an earlier create that has not finished
		// (Proxmox adds the VM to the pool at the end), or someone else's guest. Never pick another
		// VMID for this name: that is how duplicates are made.
		return fmt.Errorf("VM %s: VMID %d, reserved for it, is in use by a guest Mamela cannot see in pool %s. "+
			"If an earlier create of %s is still running, wait for it and apply again; otherwise remove the "+
			"other guest from VMID %d or release the reservation by hand: %w",
			name, vmid, p.Config.Pool, name, vmid, reconcile.ErrUnknownOutcome)
	}

	volume, err := p.ensureImage(ctx, spec.Image, img)
	if err != nil {
		return fmt.Errorf("VM %s: %w", name, err)
	}
	params := url.Values{
		"vmid":        {strconv.Itoa(vmid)},
		"name":        {name},
		"pool":        {p.Config.Pool},
		"tags":        {p.Config.Tag},
		"description": {marker{Name: name, Image: spec.Image, State: stateCreating}.String()},
		"ostype":      {"l26"},
		"cores":       {strconv.FormatInt(spec.CPU, 10)},
		"memory":      {strconv.FormatInt(spec.MemoryMiB, 10)},
		"scsihw":      {"virtio-scsi-single"},
		"scsi0":       {p.Config.DiskStorage + ":0,import-from=" + volume + ",discard=on"},
		"ide2":        {p.Config.DiskStorage + ":cloudinit"},
		"boot":        {"order=scsi0"},
		"serial0":     {"socket"},
		"vga":         {"serial0"},
		"net0":        {"virtio,bridge=" + p.Config.Bridge},
		"ipconfig0":   {"ip=dhcp"},
		"ciuser":      {p.Config.User},
		"onboot":      {"0"},
	}
	if keys := strings.TrimSpace(p.Config.SSHPublicKeys); keys != "" {
		// Proxmox expects the keys URL-encoded inside the (also encoded) form value.
		params.Set("sshkeys", strings.ReplaceAll(url.QueryEscape(keys+"\n"), "+", "%20"))
	}
	upid, err := p.API.CreateVM(ctx, p.Config.Node, params)
	if err != nil {
		return writeErr(fmt.Sprintf("create VM %s (VMID %d)", name, vmid), err)
	}
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
	if st.Status != "running" {
		upid, err := p.API.StartVM(ctx, p.Config.Node, vmid)
		if err != nil {
			return writeErr(fmt.Sprintf("start VM %s", name), err)
		}
		if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
			return fmt.Errorf("start VM %s: %w", name, err)
		}
	}
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
		if err := p.API.SetDescription(ctx, p.Config.Node, vmid, m.String(), cfg.String("digest")); err != nil {
			return writeErr(fmt.Sprintf("mark VM %s ready", name), err)
		}
	}
	return nil
}

// grow extends the disk of a managed, ready VM.
func (p *Provider) grow(ctx context.Context, name string, spec graph.VirtualMachineSpec) error {
	vmid, ok, err := p.VMIDs.VMID(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("VM %s has no VMID reserved by Mamela; it is not managed", name)
	}
	vms, err := p.scan(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if v.vmid == vmid && v.owned && v.marker.Name == name {
			return p.growTo(ctx, vmid, name, spec.DiskGiB)
		}
	}
	return fmt.Errorf("VM %s (VMID %d) is not a managed VM in pool %s", name, vmid, p.Config.Pool)
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

// wait polls a task until it stops. A task that does not finish in time, or whose status cannot
// be read, has an unknown outcome.
func (p *Provider) wait(ctx context.Context, upid string, timeout time.Duration) error {
	interval := p.Config.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	start := p.Clock()
	for {
		t, err := p.API.TaskStatus(ctx, p.Config.Node, upid)
		if err != nil {
			return unknownErr("read task "+upid, err)
		}
		if t.Status == "stopped" {
			if t.Succeeded() {
				return nil
			}
			return fmt.Errorf("task failed: %s", t.ExitStatus)
		}
		if p.Clock().Sub(start) > timeout {
			return fmt.Errorf("task %s still running after %s: %w", upid, timeout, reconcile.ErrUnknownOutcome)
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
		return 20 * time.Minute
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
