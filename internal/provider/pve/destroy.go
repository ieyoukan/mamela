package pve

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/proxmox"
)

// Destroyer is the API `mamela destroy` uses: the provider's API plus stopping and destroying a
// VM. The provider and the reconciler only ever get an API, which cannot destroy anything.
type Destroyer interface {
	API
	StopVM(ctx context.Context, node string, vmid int) (string, error)
	DestroyVM(ctx context.Context, node string, vmid int) (string, error)
}

// Target is a VM that destroy would remove, as observed.
type Target struct {
	Name    string
	VMID    int
	State   string   // marker state: creating or ready
	Status  string   // running, stopped
	Volumes []string // its disks and attached images ("scsi0 local-lvm:vm-7001-disk-0,..."), for the person to check
	Seeds   []string // cloud-init seed ISOs it uses (left on the storage; see mamela lab cleanup)
}

// DestroyCalls are the calls of `mamela destroy`, for the permission catalog.
var DestroyCalls = []proxmox.Call{
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/stop", When: "mamela destroy, if the VM is running", Maybe: true},
	{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Params: []string{"destroy-unreferenced-disks"}, When: "mamela destroy"},
	{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
	{Method: "GET", Path: "/cluster/nextid", Params: []string{"vmid"}, When: "mamela destroy: confirm the VMID is free"},
}

// DestroyTarget finds the VM Mamela created for name and describes it. Read-only. Only a VM that
// is Mamela's in every respect qualifies: in the pool and the VMID range, with Mamela's marker
// for this name, and the VMID reserved for this name in Mamela's database (a VM still being
// created qualifies; one that lost its tag or reservation does not).
func (p *Provider) DestroyTarget(ctx context.Context, name string) (Target, error) {
	vmid, ok, err := p.VMIDs.VMID(ctx, name)
	if err != nil {
		return Target{}, err
	}
	if !ok {
		return Target{}, fmt.Errorf("Mamela has no VM %s (no VMID reserved for it); it destroys only VMs it created", name)
	}
	vms, err := p.scan(ctx)
	if err != nil {
		return Target{}, err
	}
	i := slices.IndexFunc(vms, func(v lab) bool { return v.vmid == vmid })
	if i < 0 {
		return Target{}, fmt.Errorf("VM %s (VMID %d) is not visible in pool %s (gone, or a create still running); nothing to destroy", name, vmid, p.Config.Pool)
	}
	v := vms[i]
	if !v.owned || v.marker.Name != name {
		return Target{}, fmt.Errorf("VM %d in pool %s is not Mamela's VM %s (marker %q, tag present %v); Mamela will not destroy it",
			vmid, p.Config.Pool, name, v.marker.Name, v.tagged)
	}
	st, err := p.API.VMStatus(ctx, p.Config.Node, vmid)
	if err != nil {
		return Target{}, fmt.Errorf("read the status of VM %s: %w", name, err)
	}
	t := Target{Name: name, VMID: vmid, State: v.marker.State, Status: st.Status}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range v.config {
			if !yield(k) {
				return
			}
		}
	}) {
		if !isDrive(k) {
			continue
		}
		val := v.config.String(k)
		t.Volumes = append(t.Volumes, k+" "+val)
		if strings.Contains(val, ":iso/mamela-seed-") {
			vol, _, _ := strings.Cut(val, ",")
			t.Seeds = append(t.Seeds, vol)
		}
	}
	return t, nil
}

func isDrive(k string) bool {
	for _, p := range []string{"scsi", "ide", "sata", "virtio", "efidisk", "tpmstate", "unused"} {
		if rest, ok := strings.CutPrefix(k, p); ok && rest != "" && strings.Trim(rest, "0123456789") == "" {
			return true
		}
	}
	return false
}

// Destroy stops (if running) and destroys the target, then checks that its VMID is free. The
// caller has had a person confirm the target; Destroy looks at it again first and refuses if it
// changed in between.
func (p *Provider) Destroy(ctx context.Context, d Destroyer, want Target) error {
	ctx = proxmox.RestrictOperation(ctx, "destroy", append(slices.Clone(ObserveCalls), DestroyCalls...))
	ctx = proxmox.Bind(ctx, map[string]string{"node": p.Config.Node, "vmid": strconv.Itoa(want.VMID)})
	now, err := p.DestroyTarget(ctx, want.Name)
	if err != nil {
		return err
	}
	if now.VMID != want.VMID || !slices.Equal(now.Volumes, want.Volumes) {
		return fmt.Errorf("VM %s changed since it was shown (VMID %d, volumes %v); nothing was destroyed, run destroy again", want.Name, now.VMID, now.Volumes)
	}
	if now.Status == "running" {
		upid, err := d.StopVM(ctx, p.Config.Node, now.VMID)
		if err != nil {
			return fmt.Errorf("stop VM %s: %w", want.Name, err)
		}
		if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
			return fmt.Errorf("stop VM %s: %w", want.Name, err)
		}
	}
	upid, err := d.DestroyVM(ctx, p.Config.Node, now.VMID)
	if err != nil {
		return fmt.Errorf("destroy VM %s (VMID %d): %w", want.Name, now.VMID, err)
	}
	if err := p.wait(ctx, upid, p.taskTimeout()); err != nil {
		return fmt.Errorf("destroy VM %s (VMID %d): %w", want.Name, now.VMID, err)
	}
	free, err := p.API.VMIDFree(ctx, now.VMID)
	if err != nil {
		return fmt.Errorf("check that VMID %d is free: %w", now.VMID, err)
	}
	if !free {
		return fmt.Errorf("VMID %d is still in use after destroying VM %s", now.VMID, want.Name)
	}
	return nil
}

// Rebooter is the API `mamela reboot` uses: the provider's API plus resetting a VM.
type Rebooter interface {
	API
	ResetVM(ctx context.Context, node string, vmid int) (string, error)
}

// RebootCalls are the calls of `mamela reboot`, for the permission catalog.
var RebootCalls = []proxmox.Call{
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot", When: "mamela reboot", Maybe: true},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reset", When: "mamela reboot --hard", Maybe: true},
	{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
}

// rebootTimeout bounds a guest's own shutdown: a guest that does not respond (for example one in
// emergency mode, where nothing handles the ACPI request) needs --hard.
const rebootTimeout = 5 * time.Minute

// Reboot restarts a running VM Mamela created: by asking the guest (a clean shutdown, then a
// start), or with hard, by resetting it at once. It looks at the VM again first and refuses if it
// is not the one shown. A stopped VM is started by plan and apply (start-vm), not here.
func (p *Provider) Reboot(ctx context.Context, r Rebooter, want Target, hard bool) error {
	ctx = proxmox.RestrictOperation(ctx, "reboot", append(slices.Clone(ObserveCalls), RebootCalls...))
	ctx = proxmox.Bind(ctx, map[string]string{"node": p.Config.Node, "vmid": strconv.Itoa(want.VMID)})
	now, err := p.DestroyTarget(ctx, want.Name)
	if err != nil {
		return err
	}
	if now.VMID != want.VMID || !slices.Equal(now.Volumes, want.Volumes) {
		return fmt.Errorf("VM %s changed since it was shown (VMID %d, volumes %v); nothing was done, run reboot again", want.Name, now.VMID, now.Volumes)
	}
	if now.Status != "running" {
		return fmt.Errorf("VM %s is %s, not running; plan and apply start it (start-vm)", want.Name, now.Status)
	}
	if hard {
		upid, err := r.ResetVM(ctx, p.Config.Node, now.VMID)
		if err != nil {
			return fmt.Errorf("reset VM %s: %w", want.Name, err)
		}
		return p.wait(ctx, upid, rebootTimeout)
	}
	upid, err := r.RebootVM(ctx, p.Config.Node, now.VMID)
	if err != nil {
		return fmt.Errorf("reboot VM %s: %w", want.Name, err)
	}
	if err := p.wait(ctx, upid, rebootTimeout); err != nil {
		return fmt.Errorf("reboot VM %s: %w (a guest that does not respond needs --hard)", want.Name, err)
	}
	return nil
}
