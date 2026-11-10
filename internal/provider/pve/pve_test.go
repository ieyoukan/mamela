package pve_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/proxmox"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/store"
)

var imageBytes = append([]byte{'Q', 'F', 'I', 0xfb}, []byte("a tiny fake cloud image")...)

func testImage() pve.Image {
	sum := sha256.Sum256(imageBytes)
	return pve.Image{URL: "https://images.invalid/test.img", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(imageBytes)), MinDiskGiB: 4}
}

type memImages struct{ opens int }

func (m *memImages) Open(context.Context, pve.Image) (pve.ImageFile, error) {
	m.opens++
	return io.NopCloser(bytes.NewReader(imageBytes)), nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

type env struct {
	t      *testing.T
	ctx    context.Context
	fake   *fakePVE
	store  *store.Store
	clock  *clock
	images *memImages
	p      *pve.Provider
	api    *proxmox.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake, srv := newFakePVE(t)
	sum := sha256.Sum256(srv.Certificate().Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	api, err := proxmox.New(srv.URL, "mamela@pve!lab", "secret", proxmox.Options{Fingerprint: strings.Join(parts, ":"), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), c.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	images := &memImages{}
	p := &pve.Provider{
		Config: pve.Config{
			Node: "n1", Pool: "lab", Tag: "mamela-lab", VMIDMin: 7000, VMIDMax: 7009,
			DiskStorage: "local-lvm", ImageStorage: "local", Bridge: "vmbr0",
			MaxVMs: 2, MaxCPU: 2, MaxMemoryMiB: 2048, MaxDiskGiB: 20,
			User: "mamela", SSHPublicKeys: "ssh-ed25519 AAAAC3Nza+test/key= alice@laptop",
			Catalog:     map[string]pve.Image{"ubuntu-24.04": testImage()},
			TaskTimeout: time.Minute,
		},
		API: api, VMIDs: st, Images: images, Clock: c.Now, Sleep: c.Sleep,
	}
	return &env{t: t, ctx: context.Background(), fake: fake, store: st, clock: c, images: images, p: p, api: api}
}

func vmResource(name string, cpu, mem, disk int64) graph.Resource {
	return graph.Resource{Kind: graph.KindVirtualMachine, Name: name,
		VirtualMachine: &graph.VirtualMachineSpec{CPU: cpu, MemoryMiB: mem, DiskGiB: disk, Image: "ubuntu-24.04"}}
}

func (e *env) apply(op policy.Op, r graph.Resource) error {
	return e.p.Apply(e.ctx, reconcile.Step{OperationID: "op-test", Op: op, Resource: r})
}

func (e *env) writes() int {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return e.fake.methods["POST"] + e.fake.methods["PUT"]
}

func TestCreatesVMFromImageWithoutTemplate(t *testing.T) {
	e := newEnv(t)
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	f := e.fake
	if f.creates != 1 || f.uploads != 1 || f.starts != 1 || f.resizes != 1 {
		t.Fatalf("creates %d uploads %d starts %d resizes %d, want 1 each", f.creates, f.uploads, f.starts, f.resizes)
	}
	vm := f.vms[7000]
	if vm == nil {
		t.Fatal("no VM 7000")
	}
	want := map[string]string{
		"name": "web", "tags": "mamela-lab", "cores": "1", "memory": "1024", "ciuser": "mamela",
		"ide2": "local-lvm:cloudinit", "net0": "virtio,bridge=vmbr0", "ipconfig0": "ip=dhcp",
		"scsi0":   "local-lvm:vm-7000-disk-0,discard=on,size=10G",
		"sshkeys": "ssh-ed25519%20AAAAC3Nza%2Btest%2Fkey%3D%20alice%40laptop%0A",
	}
	for k, v := range want {
		if vm.config[k] != v {
			t.Errorf("config %s = %q, want %q", k, vm.config[k], v)
		}
	}
	if keys, _ := url.QueryUnescape(vm.config["sshkeys"]); keys != "ssh-ed25519 AAAAC3Nza+test/key= alice@laptop\n" {
		t.Errorf("sshkeys decode to %q", keys)
	}
	if vm.pool != "lab" || vm.status != "running" || !strings.Contains(vm.config["description"], "mamela-state: ready") {
		t.Errorf("pool %q status %q description %q", vm.pool, vm.status, vm.config["description"])
	}

	snap, err := e.p.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := snap.VMs["web"]
	if !got.Managed || got.CPU != 1 || got.MemoryMiB != 1024 || got.DiskGiB != 10 || got.Image != "ubuntu-24.04" {
		t.Fatalf("observed %+v", got)
	}

	// A second VM reuses the uploaded image and gets the next VMID.
	if err := e.apply(policy.OpCreateVM, vmResource("db", 1, 512, 8)); err != nil {
		t.Fatal(err)
	}
	if f.uploads != 1 || e.images.opens != 1 || f.vms[7001] == nil {
		t.Fatalf("uploads %d opens %d vm7001 %v", f.uploads, e.images.opens, f.vms[7001] != nil)
	}
}

// The reconciler, the store and the provider together: plan, approve, apply, then in sync.
func TestPlanApplyConverges(t *testing.T) {
	e := newEnv(t)
	g := &graph.Graph{Resources: []graph.Resource{vmResource("web", 1, 1024, 10)}}
	run := func() (*plan.Plan, *reconcile.Report) {
		snap, err := e.p.Observe(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := plan.Build(g, snap, policy.Default)
		if err := e.store.SavePlan(e.ctx, g, snap, policy.Default, p); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Approve(e.ctx, p.Digest, "alice", time.Hour); err != nil {
			t.Fatal(err)
		}
		rec := &reconcile.Reconciler{Store: e.store, Provider: e.p, Owner: "test", Lease: time.Hour}
		rep, err := rec.Apply(e.ctx, p.Digest)
		if err != nil {
			t.Fatal(err)
		}
		return p, rep
	}
	_, rep := run()
	if rep.Stopped != nil || len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("first apply: %+v %+v", rep.Steps, rep.Stopped)
	}
	p, rep := run()
	if len(rep.Steps) != 0 || p.Changes[0].Status != plan.StatusInSync {
		t.Fatalf("second plan is not in sync: %+v", p.Changes)
	}
}

// The create request reaches Proxmox but its answer is lost. The next run must find the VM by its
// reserved VMID and finish it, not create a second one.
// The answer to the create is lost. Mamela finds its create task in Proxmox's task list and
// carries on with it in the same run: no unknown outcome and no second create.
func TestLostCreateResponseIsFoundInTheTaskList(t *testing.T) {
	e := newEnv(t)
	e.fake.loseCreateResponse = true
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatalf("want the create found and finished, got %v", err)
	}
	if e.fake.creates != 1 || len(e.fake.vms) != 1 || e.fake.vms[7000].status != "running" {
		t.Fatalf("creates %d, VMs %d", e.fake.creates, len(e.fake.vms))
	}
}

// Reading a task's status fails now and then (a busy node). That is not an unknown outcome.
func TestTaskStatusBlipsAreTolerated(t *testing.T) {
	e := newEnv(t)
	e.fake.taskStatusErrors = 3
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatalf("want success despite 3 failed status reads, got %v", err)
	}
	// A status that cannot be read for long is unknown, and the next run waits for the same task.
	e.fake.taskStatusErrors = 100
	err := e.apply(policy.OpCreateVM, vmResource("db", 1, 512, 8))
	if !errors.Is(err, reconcile.ErrUnknownOutcome) {
		t.Fatalf("want an unknown outcome, got %v", err)
	}
	e.fake.taskStatusErrors = 0
	if err := e.apply(policy.OpCreateVM, vmResource("db", 1, 512, 8)); err != nil {
		t.Fatal(err)
	}
	if e.fake.creates != 2 {
		t.Fatalf("creates %d, want 2", e.fake.creates)
	}
}

// The create task is still running (the VM is locked and not yet in the pool, so the token cannot
// see it). Retrying must neither create again nor move to another VMID.
func TestCreateStillRunningIsNeverDuplicated(t *testing.T) {
	e := newEnv(t)
	e.fake.loseCreateResponse = true
	e.fake.createStaysRunning = true
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); !errors.Is(err, reconcile.ErrUnknownOutcome) {
		t.Fatalf("want an unknown outcome, got %v", err)
	}
	for range 3 {
		err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10))
		if !errors.Is(err, reconcile.ErrUnknownOutcome) || !strings.Contains(err.Error(), "VMID 7000") ||
			!strings.Contains(err.Error(), "still running") {
			t.Fatalf("retry: want to keep waiting for the create of VMID 7000, got %v", err)
		}
	}
	if e.fake.creates != 1 || len(e.fake.vms) != 1 {
		t.Fatalf("creates %d, VMs %d", e.fake.creates, len(e.fake.vms))
	}
	// The task ends; the next run finishes the same VM.
	e.fake.mu.Lock()
	e.fake.vms[7000].lock, e.fake.vms[7000].pool = "", "lab"
	e.fake.mu.Unlock()
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if e.fake.creates != 1 || e.fake.vms[7000].status != "running" {
		t.Fatalf("creates %d status %s", e.fake.creates, e.fake.vms[7000].status)
	}
}

func TestFailedCreateIsRetriedOnTheSameVMID(t *testing.T) {
	e := newEnv(t)
	e.fake.createTaskFails = true
	err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10))
	if err == nil || errors.Is(err, reconcile.ErrUnknownOutcome) || !strings.Contains(err.Error(), "import failed") {
		t.Fatalf("want a definite failure with Proxmox's reason, got %v", err)
	}
	e.fake.createTaskFails = false
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.vms) != 1 || e.fake.vms[7000] == nil {
		t.Fatalf("VMs: %v", e.fake.sorted())
	}
}

func TestReservedVMIDTakenBySomeoneElse(t *testing.T) {
	e := newEnv(t)
	e.fake.hidden[7000] = true // already in use when the VMID is reserved: skipped
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if e.fake.vms[7001] == nil {
		t.Fatal("did not skip the used VMID")
	}

	// Reserved first, taken afterwards: Mamela stops and never picks another VMID for the name.
	if _, err := e.store.AllocateVMID(e.ctx, "db", 7002, 7002, func(context.Context, int) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	e.fake.hidden[7002] = true
	before := e.fake.creates
	err := e.apply(policy.OpCreateVM, vmResource("db", 1, 1024, 10))
	if err == nil || errors.Is(err, reconcile.ErrUnknownOutcome) || !strings.Contains(err.Error(), "VMID 7002") ||
		!strings.Contains(err.Error(), "no create task of this token") {
		t.Fatalf("want a definite refusal about VMID 7002, got %v", err)
	}
	if e.fake.creates != before {
		t.Fatal("created a VM although its reserved VMID is taken")
	}
	if v, _, _ := e.store.VMID(e.ctx, "db"); v != 7002 {
		t.Fatalf("reservation moved to %d", v)
	}
}

func TestRefusesBeforeSendingAnything(t *testing.T) {
	e := newEnv(t)
	other := vmResource("web", 1, 1024, 10)
	other.VirtualMachine.Image = "debian-12"
	for name, tc := range map[string]struct {
		op policy.Op
		r  graph.Resource
	}{
		"too many vCPU":     {policy.OpCreateVM, vmResource("web", 4, 1024, 10)},
		"too much memory":   {policy.OpCreateVM, vmResource("web", 1, 4096, 10)},
		"disk over budget":  {policy.OpCreateVM, vmResource("web", 1, 1024, 50)},
		"disk under image":  {policy.OpCreateVM, vmResource("web", 1, 1024, 2)},
		"image not pinned":  {policy.OpCreateVM, other},
		"shrink":            {policy.OpShrinkVMDisk, vmResource("web", 1, 1024, 10)},
		"replace image":     {policy.OpReplaceVMImage, vmResource("web", 1, 1024, 10)},
		"resize cpu":        {policy.OpResizeVMCPU, vmResource("web", 2, 1024, 10)},
		"change foreign":    {policy.OpChangeForeign, vmResource("web", 1, 1024, 10)},
		"packages are not ": {policy.OpInstallPackage, graph.Resource{Kind: graph.KindNodePackage, Name: "web/nginx", NodePackage: &graph.NodePackageSpec{Node: "web", Package: "nginx", State: graph.PackagePresent}}},
	} {
		if err := e.apply(tc.op, tc.r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := e.writes(); n != 0 {
		t.Fatalf("%d write requests were sent", n)
	}
}

func TestVMBudgetCountsManagedVMs(t *testing.T) {
	e := newEnv(t)
	e.p.Config.MaxVMs = 1
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(policy.OpCreateVM, vmResource("db", 1, 1024, 10)); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("want a budget refusal, got %v", err)
	}
}

func TestGrowsButNeverShrinks(t *testing.T) {
	e := newEnv(t)
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(policy.OpGrowVMDisk, vmResource("web", 1, 1024, 12)); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.vms[7000].config["scsi0"]; !strings.HasSuffix(got, "size=12G") {
		t.Fatalf("scsi0 %s", got)
	}
	resizes := e.fake.resizes
	// A smaller size asks for nothing: Proxmox is not even asked.
	if err := e.apply(policy.OpGrowVMDisk, vmResource("web", 1, 1024, 8)); err != nil {
		t.Fatal(err)
	}
	if e.fake.resizes != resizes {
		t.Fatal("sent a resize to a smaller size")
	}
}

func TestOnlyFullyMarkedVMsAreManaged(t *testing.T) {
	e := newEnv(t)
	marker := "Managed by Mamela. Do not edit the lines below.\nmamela-name: %s\nmamela-image: ubuntu-24.04\nmamela-state: ready\n"
	add := func(vmid int, name, tags, description string) {
		e.fake.vms[vmid] = &fakeVM{vmid: vmid, pool: "lab", status: "running", config: map[string]string{
			"name": name, "tags": tags, "description": description, "cores": "1", "memory": "1024",
			"scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,size=10G", vmid)}}
	}
	add(7005, "hand-made", "mamela-lab", "made by a person")                 // no marker
	add(7006, "copied", "mamela-lab", fmt.Sprintf(marker, "copied"))         // marker but no reservation
	add(8000, "outside", "mamela-lab", fmt.Sprintf(marker, "outside"))       // outside the VMID range
	add(7007, "untagged", "something-else", fmt.Sprintf(marker, "untagged")) // without the tag
	free := func(context.Context, int) (bool, error) { return true, nil }
	if _, err := e.store.AllocateVMID(e.ctx, "outside", 8000, 8000, free); err != nil {
		t.Fatal(err)
	}
	// Reserved, marked ready and in range, but someone removed the tag: no longer Mamela's.
	if _, err := e.store.AllocateVMID(e.ctx, "untagged", 7007, 7007, free); err != nil {
		t.Fatal(err)
	}

	snap, err := e.p.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hand-made", "copied", "outside", "untagged"} {
		vm, ok := snap.VMs[name]
		if !ok || vm.Managed {
			t.Errorf("%s: present %v managed %v, want present and Foreign", name, ok, vm.Managed)
		}
	}
	// A marked VM without a reservation (another or a lost database) blocks creating the name again.
	if err := e.apply(policy.OpCreateVM, vmResource("copied", 1, 1024, 10)); err == nil || !strings.Contains(err.Error(), "reservation") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if e.fake.creates != 0 {
		t.Fatal("created a VM")
	}
}

// Proxmox refuses tags from a pool-scoped token until the create task has put the VM in the pool,
// so a VM being created has no tag yet. It is still ours (marker "creating" and the reservation),
// and finishing it adds the tag together with the ready marker.
func TestUntaggedVMBeingCreatedIsResumedAndTagged(t *testing.T) {
	e := newEnv(t)
	free := func(context.Context, int) (bool, error) { return true, nil }
	if _, err := e.store.AllocateVMID(e.ctx, "web", 7000, 7000, free); err != nil {
		t.Fatal(err)
	}
	e.fake.vms[7000] = &fakeVM{vmid: 7000, pool: "lab", status: "stopped", config: map[string]string{
		"name": "web", "cores": "1", "memory": "1024", "scsi0": "local-lvm:vm-7000-disk-0,size=3584M",
		"description": "Managed by Mamela. Do not edit the lines below.\nmamela-name: web\nmamela-image: ubuntu-24.04\nmamela-state: creating\n"}}

	snap, err := e.p.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.VMs["web"]; ok {
		t.Fatalf("a VM being created is observed as %+v, want absent so that create resumes it", snap.VMs["web"])
	}
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	vm := e.fake.vms[7000]
	if e.fake.creates != 0 || vm.config["tags"] != "mamela-lab" || !strings.Contains(vm.config["description"], "mamela-state: ready") {
		t.Fatalf("creates %d tags %q description %q", e.fake.creates, vm.config["tags"], vm.config["description"])
	}
}

// Found in L1: without Pool.Audit, Proxmox leaves the pool out of /cluster/resources. Mamela must
// say so instead of taking its own VM for someone else's and planning to create it again.
func TestPoolHiddenFromTheTokenIsAnError(t *testing.T) {
	e := newEnv(t)
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	e.fake.noPoolAudit = true
	if _, err := e.p.Observe(e.ctx); err == nil || !strings.Contains(err.Error(), "Pool.Audit") {
		t.Fatalf("want an error about Pool.Audit, got %v", err)
	}
}

func TestImageCacheVerifiesDownloads(t *testing.T) {
	img := testImage()
	body := imageBytes
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write(body)
	}))
	defer srv.Close()
	img.URL = srv.URL + "/image.img"
	dir := t.TempDir()
	cache := &pve.ImageCache{Dir: dir}
	ctx := context.Background()

	f, err := cache.Open(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, imageBytes) {
		t.Fatal("wrong content")
	}
	if f, err := cache.Open(ctx, img); err != nil || requests != 1 {
		t.Fatalf("second open: %v, %d requests", err, requests)
	} else {
		f.Close()
	}

	// A tampered cache file is refused, not uploaded.
	path := filepath.Join(dir, img.SHA256+".qcow2")
	os.WriteFile(path, append([]byte{'Q', 'F', 'I', 0xfb}, []byte("a tiny fake cloud imagX")...), 0o600)
	if _, err := cache.Open(ctx, img); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("tampered cache: %v", err)
	}

	// A download with the wrong content is refused and not kept.
	os.Remove(path)
	body = []byte("something else entirely, the same length!")[:len(imageBytes)]
	if _, err := cache.Open(ctx, img); err == nil {
		t.Fatal("accepted a download with the wrong checksum")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left files behind: %v", entries)
	}
}

// fakeSeeds makes seeds and remembers them, like seed.Maker over the store.
type fakeSeeds struct {
	made     []string
	reusable string
}

func (s *fakeSeeds) Reusable(context.Context, string, int) (string, bool, error) {
	return s.reusable, s.reusable != "", nil
}

func (s *fakeSeeds) New(_ context.Context, node string, vmid int) (string, []byte, error) {
	name := fmt.Sprintf("mamela-seed-%s-%d-%d.iso", node, vmid, len(s.made))
	s.made = append(s.made, name)
	s.reusable = name
	return name, []byte("seed image " + name), nil
}

func TestCreatesVMWithMameletSeed(t *testing.T) {
	e := newEnv(t)
	seeds := &fakeSeeds{}
	e.p.Seeds = seeds
	e.fake.createTaskFails = true // the first create fails after the seed was uploaded
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err == nil {
		t.Fatal("want the failed create")
	}
	e.fake.createTaskFails = false
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	if len(seeds.made) != 1 || e.fake.uploads != 2 {
		t.Fatalf("seeds %v, uploads %d: the retry must reuse the seed", seeds.made, e.fake.uploads)
	}
	cfg := e.fake.vms[7000].config
	if cfg["ide2"] != "local:iso/"+seeds.made[0]+",media=cdrom" || cfg["agent"] != "enabled=1" {
		t.Fatalf("ide2 %q agent %q", cfg["ide2"], cfg["agent"])
	}
	for _, k := range []string{"ciuser", "sshkeys", "ipconfig0"} {
		if _, ok := cfg[k]; ok {
			t.Errorf("%s set although the seed provides it", k)
		}
	}

	// A seed that is no longer on the storage is made again.
	delete(e.fake.volumes, "local:iso/"+seeds.made[0])
	if err := e.apply(policy.OpCreateVM, vmResource("db", 1, 512, 8)); err != nil {
		t.Fatal(err)
	}
	if len(seeds.made) != 2 {
		t.Fatalf("seeds %v", seeds.made)
	}
}

// The operating token's grants are derived from the permission catalog (lab.Grants). Against a
// fake Proxmox that checks permissions the way Proxmox VE 9.2 does (written from its source, not
// from the catalog), they must be enough for everything Mamela does, and every single privilege
// must be needed: without it, something fails.
func TestDerivedGrantsAreEnoughAndEachIsNeeded(t *testing.T) {
	for _, withSeed := range []bool{true, false} {
		site := &lab.Site{Proxmox: lab.Proxmox{Node: "n1"}, Boundary: lab.Boundary{Pool: "lab", VMIDMin: 7000, VMIDMax: 7009, Tag: "mamela-lab"},
			Placement: lab.Placement{DiskStorage: "local-lvm", ImageStorage: "local", Bridge: "vmbr0", SDNZone: "localnetwork"}}
		if withSeed {
			site.ControlPlane = lab.ControlPlane{URL: "https://127.0.0.1:8443"}
		}
		grants := map[string][]string{}
		var all [][2]string
		for _, g := range site.Grants() {
			grants[g.Path] = g.Privs
			for _, p := range g.Privs {
				all = append(all, [2]string{g.Path, p})
			}
		}
		everything := func(acl map[string][]string) error {
			e := newEnv(t)
			e.fake.acl = acl
			if withSeed {
				e.p.Seeds = &fakeSeeds{}
			}
			if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
				return fmt.Errorf("create: %w", err)
			}
			snap, err := e.p.Observe(e.ctx)
			if err != nil {
				return fmt.Errorf("observe: %w", err)
			}
			if vm, ok := snap.VMs["web"]; !ok || !vm.Managed {
				return fmt.Errorf("observe: web not seen as managed (%+v)", snap.VMs)
			}
			if err := e.apply(policy.OpGrowVMDisk, vmResource("web", 1, 1024, 12)); err != nil {
				return fmt.Errorf("grow: %w", err)
			}
			if err := e.apply(policy.OpResizeVMCPU, vmResource("web", 2, 2048, 12)); err != nil {
				return fmt.Errorf("resize: %w", err)
			}
			if ips, err := e.api.GuestIPv4(e.ctx, "n1", 7000); err != nil || len(ips) != 1 {
				return fmt.Errorf("guest addresses: %v %v", ips, err)
			}
			return nil
		}
		if err := everything(grants); err != nil {
			t.Fatalf("seed %v: the derived grants are not enough: %v", withSeed, err)
		}
		for _, drop := range all {
			acl := map[string][]string{}
			for path, privs := range grants {
				acl[path] = slices.DeleteFunc(slices.Clone(privs), func(p string) bool { return path == drop[0] && p == drop[1] })
			}
			if err := everything(acl); err == nil {
				t.Errorf("seed %v: %s on %s is granted but nothing needs it", withSeed, drop[1], drop[0])
			}
		}
	}
}

// A 403 from Proxmox says what Mamela knows: a privilege in the catalog that the token lacks.
func TestPermissionErrorsAreExplained(t *testing.T) {
	e := newEnv(t)
	e.fake.acl = map[string][]string{"/pool/lab": {"VM.Audit", "Pool.Audit"}, "/storage/local": {"Datastore.Audit"}}
	err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10))
	if err == nil || !strings.Contains(err.Error(), "the operating token lacks Datastore.AllocateTemplate on /storage/local") ||
		!strings.Contains(err.Error(), "upload the pinned cloud image") {
		t.Fatalf("got %v", err)
	}
}

// OperationCalls and ObserveCalls (what the permission catalog is told) must cover every request
// the provider really sends, including the paths for lost answers and retries.
func TestCallsCoverWhatTheProviderSends(t *testing.T) {
	declared := func(calls []proxmox.Call) map[string]bool {
		out := map[string]bool{}
		for _, c := range calls {
			out[c.Method+" "+c.Path] = true
		}
		return out
	}
	check := func(name string, e *env, allowed map[string]bool) {
		t.Helper()
		for _, r := range e.fake.requests {
			method, path, _ := strings.Cut(r, " ")
			ep, ok := proxmox.LookupEndpoint(method, strings.TrimPrefix(path, "/api2/json"))
			if !ok {
				t.Errorf("%s: %s is not in the catalog", name, r)
				continue
			}
			if !allowed[ep.Method+" "+ep.Path] {
				t.Errorf("%s: sends %s %s, which OperationCalls/ObserveCalls do not list", name, ep.Method, ep.Path)
			}
		}
	}
	for _, withSeed := range []bool{true, false} {
		e := newEnv(t)
		if withSeed {
			e.p.Seeds = &fakeSeeds{}
		}
		e.fake.loseCreateResponse = true
		calls, _ := pve.OperationCalls(policy.OpCreateVM, withSeed)
		allowed := declared(append(calls, pve.ObserveCalls...))
		if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.p.Observe(e.ctx); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("create (seed %v)", withSeed), e, allowed)
		got := []string{}
		for k := range e.fake.vms[7000].config {
			got = append(got, k)
		}
		for _, k := range pve.CreateParamNames(withSeed) {
			if k != "vmid" && k != "pool" && !slices.Contains(got, k) {
				t.Errorf("CreateParamNames lists %s, which was not sent", k)
			}
		}

		e.fake.requests = nil
		grow, _ := pve.OperationCalls(policy.OpGrowVMDisk, withSeed)
		if err := e.apply(policy.OpGrowVMDisk, vmResource("web", 1, 1024, 12)); err != nil {
			t.Fatal(err)
		}
		check("grow", e, declared(append(grow, pve.ObserveCalls...)))

		e.fake.requests = nil
		resize, _ := pve.OperationCalls(policy.OpResizeVMCPU, withSeed)
		if err := e.apply(policy.OpResizeVMCPU, vmResource("web", 2, 2048, 12)); err != nil {
			t.Fatal(err)
		}
		check("resize", e, declared(append(resize, pve.ObserveCalls...)))
	}
	if _, ok := pve.OperationCalls(policy.OpShrinkVMDisk, true); ok {
		t.Error("claims calls for an operation the provider does not carry out")
	}
}

// Resizing sets whatever differs in one change and restarts a running VM once to apply it; the
// second resize operation of the same change finds nothing left to do.
func TestResizeRestartsOnce(t *testing.T) {
	e := newEnv(t)
	if err := e.apply(policy.OpCreateVM, vmResource("web", 1, 1024, 10)); err != nil {
		t.Fatal(err)
	}
	for _, op := range []policy.Op{policy.OpResizeVMCPU, policy.OpResizeVMMemory} {
		if err := e.apply(op, vmResource("web", 2, 2048, 10)); err != nil {
			t.Fatal(err)
		}
	}
	vm := e.fake.vms[7000]
	if vm.config["cores"] != "2" || vm.config["memory"] != "2048" || e.fake.reboots != 1 {
		t.Fatalf("cores %s memory %s reboots %d", vm.config["cores"], vm.config["memory"], e.fake.reboots)
	}
	snap, _ := e.p.Observe(e.ctx)
	if got := snap.VMs["web"]; got.CPU != 2 || got.MemoryMiB != 2048 {
		t.Fatalf("observed %+v", got)
	}

	// A stopped VM takes the change without a restart.
	vm.status = "stopped"
	if err := e.apply(policy.OpResizeVMMemory, vmResource("web", 2, 1536, 10)); err != nil {
		t.Fatal(err)
	}
	if vm.config["memory"] != "1536" || e.fake.reboots != 1 {
		t.Fatalf("memory %s reboots %d", vm.config["memory"], e.fake.reboots)
	}
	// Over the budget: refused before anything is sent.
	before := e.writes()
	if err := e.apply(policy.OpResizeVMCPU, vmResource("web", 8, 1536, 10)); err == nil || e.writes() != before {
		t.Fatalf("over budget: %v", err)
	}
}
