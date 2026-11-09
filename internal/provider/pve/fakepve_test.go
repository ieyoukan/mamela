package pve_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakePVE is an in-memory Proxmox VE API for one node "n1", as seen by a token whose only VM
// privileges come from pool "lab". It models what the provider relies on: VMs are created by a
// task, join the pool only when the task ends, are invisible (403) outside the pool, refuse a
// second create of the same VMID, never shrink, and imports need the image volume to exist.
type fakePVE struct {
	t  *testing.T
	mu sync.Mutex

	vms     map[int]*fakeVM
	hidden  map[int]bool     // VMIDs used by guests the token cannot see
	volumes map[string]int64 // volid -> size
	tasks   map[string]int   // upid -> polls left until stopped
	meta    map[string]taskMeta
	failed  map[string]string
	seq     int

	imageVirtualSize string // size of a disk imported from any image, e.g. "3584M"

	// Faults.
	loseCreateResponse bool // perform the create, then drop the connection before answering
	createTaskFails    bool // the create task ends with an error (Proxmox removes the VM again)
	createStaysRunning bool // the create task never finishes; the VM stays locked outside the pool
	taskStatusErrors   int  // answer this many task status reads with 503
	noPoolAudit        bool // the token lacks Pool.Audit: /cluster/resources leaves out the pool

	// Counters.
	creates, uploads, starts, resizes int
	methods                           map[string]int
}

type taskMeta struct {
	typ   string
	vmid  int
	start int64
}

type fakeVM struct {
	vmid    int
	pool    string
	status  string
	lock    string
	config  map[string]string
	version int
}

func newFakePVE(t *testing.T) (*fakePVE, *httptest.Server) {
	f := &fakePVE{t: t, vms: map[int]*fakeVM{}, hidden: map[int]bool{}, volumes: map[string]int64{},
		tasks: map[string]int{}, meta: map[string]taskMeta{}, failed: map[string]string{}, methods: map[string]int{}, imageVirtualSize: "3584M"}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePVE) task(polls int, typ string, vmid int) string {
	f.seq++
	upid := fmt.Sprintf("UPID:n1:%08X:%s:%d:mamela@pve!lab:", f.seq, typ, vmid)
	f.tasks[upid] = polls
	f.meta[upid] = taskMeta{typ: typ, vmid: vmid, start: int64(f.seq)}
	return upid
}

// finishTasks ends every running task (as if time passed).
func (f *fakePVE) finishTasks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for upid := range f.tasks {
		f.tasks[upid] = 0
	}
}

func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"data":null,"message":%q}`, msg)
}

var (
	vmPath    = regexp.MustCompile(`^/api2/json/nodes/n1/qemu/(\d+)/(config|status/current|status/start|resize)$`)
	tasksPath = "/api2/json/nodes/n1/tasks"
	taskPath  = regexp.MustCompile(`^/api2/json/nodes/n1/tasks/([^/]+)/status$`)
	importVol = regexp.MustCompile(`^local-lvm:0,import-from=(local:import/[^,]+),discard=on$`)
)

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[r.Method]++
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut {
		f.t.Errorf("the provider sent %s %s", r.Method, r.URL.Path)
		fail(w, http.StatusNotImplemented, "not allowed")
		return
	}
	if r.Header.Get("Authorization") != "PVEAPIToken=mamela@pve!lab=secret" {
		fail(w, http.StatusUnauthorized, "no ticket")
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api2/json/cluster/resources":
		var out []map[string]any
		for _, vm := range f.sorted() {
			if vm.pool != "lab" {
				continue
			}
			e := map[string]any{"id": fmt.Sprintf("qemu/%d", vm.vmid), "type": "qemu", "vmid": vm.vmid,
				"node": "n1", "name": vm.config["name"], "status": vm.status, "pool": vm.pool, "tags": vm.config["tags"]}
			if f.noPoolAudit {
				delete(e, "pool")
			}
			out = append(out, e)
		}
		reply(w, out)
	case r.Method == http.MethodGet && p == "/api2/json/cluster/nextid":
		id, _ := strconv.Atoi(r.URL.Query().Get("vmid"))
		if _, ok := f.vms[id]; ok || f.hidden[id] {
			fail(w, http.StatusBadRequest, fmt.Sprintf("VM %d already exists", id))
			return
		}
		reply(w, strconv.Itoa(id))
	case r.Method == http.MethodGet && p == "/api2/json/nodes/n1/storage/local/content":
		var out []map[string]any
		for volid, size := range f.volumes {
			out = append(out, map[string]any{"volid": volid, "format": "qcow2", "size": size})
		}
		reply(w, out)
	case r.Method == http.MethodPost && p == "/api2/json/nodes/n1/storage/local/upload":
		f.upload(w, r)
	case r.Method == http.MethodPost && p == "/api2/json/nodes/n1/qemu":
		f.create(w, r)
	case p == tasksPath && r.Method == http.MethodGet:
		q := r.URL.Query()
		if q.Get("source") != "all" {
			f.t.Errorf("task list without source=all misses running tasks: %s", r.URL.RawQuery)
		}
		id, _ := strconv.Atoi(q.Get("vmid"))
		out := []map[string]any{}
		for upid, m := range f.meta {
			if m.vmid != id || m.typ != q.Get("typefilter") {
				continue
			}
			e := map[string]any{"upid": upid, "type": m.typ, "id": strconv.Itoa(m.vmid), "starttime": m.start, "status": "RUNNING"}
			if f.tasks[upid] == 0 {
				e["endtime"] = m.start + 1
				e["status"] = "OK"
				if msg := f.failed[upid]; msg != "" {
					e["status"] = msg
				}
			}
			out = append(out, e)
		}
		reply(w, out)
	case taskPath.MatchString(p) && r.Method == http.MethodGet && f.taskStatusErrors > 0:
		f.taskStatusErrors--
		fail(w, http.StatusServiceUnavailable, "busy")
	case taskPath.MatchString(p) && r.Method == http.MethodGet:
		upid, _ := url.PathUnescape(taskPath.FindStringSubmatch(p)[1])
		left, ok := f.tasks[upid]
		switch {
		case !ok:
			fail(w, http.StatusNotFound, "no such task")
		case left > 0:
			f.tasks[upid] = left - 1
			reply(w, map[string]any{"status": "running"})
		case f.failed[upid] != "":
			reply(w, map[string]any{"status": "stopped", "exitstatus": f.failed[upid]})
		default:
			reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
		}
	case vmPath.MatchString(p):
		m := vmPath.FindStringSubmatch(p)
		id, _ := strconv.Atoi(m[1])
		vm, ok := f.vms[id]
		if !ok || vm.pool != "lab" {
			fail(w, http.StatusForbidden, "Permission check failed")
			return
		}
		f.vm(w, r, vm, m[2])
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		fail(w, http.StatusNotImplemented, "not implemented by the fake")
	}
}

func (f *fakePVE) sorted() []*fakeVM {
	var out []*fakeVM
	for _, vm := range f.vms {
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].vmid < out[j].vmid })
	return out
}

func (f *fakePVE) upload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength <= 0 {
		fail(w, http.StatusBadRequest, "missing Content-Length") // pveproxy needs it
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	fields := map[string]string{}
	var name string
	var size int64
	sum := sha256.New()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FormName() == "filename" {
			if len(fields) != 3 {
				fail(w, http.StatusBadRequest, "the file must come after the other fields")
				return
			}
			name = part.FileName()
			size, _ = io.Copy(sum, part)
			continue
		}
		b, _ := io.ReadAll(part)
		fields[part.FormName()] = string(b)
	}
	if fields["content"] != "import" || fields["checksum-algorithm"] != "sha256" {
		fail(w, http.StatusBadRequest, "bad fields")
		return
	}
	f.uploads++
	upid := f.task(1, "imgcopy", 0)
	if hex.EncodeToString(sum.Sum(nil)) != fields["checksum"] {
		f.failed[upid] = "checksum mismatch"
	} else {
		f.volumes["local:import/"+name] = size
	}
	reply(w, upid)
}

func (f *fakePVE) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	form := r.PostForm
	id, _ := strconv.Atoi(form.Get("vmid"))
	if _, ok := f.vms[id]; ok || f.hidden[id] {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d - VM %d already exists on node 'n1'", id, id))
		return
	}
	m := importVol.FindStringSubmatch(form.Get("scsi0"))
	if m == nil {
		fail(w, http.StatusBadRequest, "scsi0: expected <storage>:0,import-from=<volume>: "+form.Get("scsi0"))
		return
	}
	if _, ok := f.volumes[m[1]]; !ok {
		fail(w, http.StatusInternalServerError, "volume "+m[1]+" does not exist")
		return
	}
	f.creates++
	// Proxmox checks tag permissions on /vms/<vmid> without the pool, at the end of the create
	// task and before it adds the VM to the pool: a pool-scoped token cannot create with tags.
	tagsAtCreate := form.Get("tags") != ""
	cfg := map[string]string{}
	for k := range form {
		if k != "vmid" && k != "pool" {
			cfg[k] = form.Get(k)
		}
	}
	cfg["scsi0"] = fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=%s", id, f.imageVirtualSize)
	vm := &fakeVM{vmid: id, status: "stopped", lock: "create", config: cfg}
	f.vms[id] = vm
	upid := f.task(2, "qmcreate", id)
	switch {
	case tagsAtCreate:
		f.failed[upid] = fmt.Sprintf("unable to create VM %d - 403 Permission check failed (/vms/%d, VM.Config.Options)", id, id)
		delete(f.vms, id)
	case f.createTaskFails:
		f.failed[upid] = "unable to create VM - import failed"
		delete(f.vms, id) // Proxmox removes a VM whose create failed
	case f.createStaysRunning:
		f.tasks[upid] = 1 << 30
	default:
		vm.lock = ""
		vm.pool = form.Get("pool") // the VM joins the pool at the end of the task
	}
	if f.loseCreateResponse {
		f.loseCreateResponse = false
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
		return
	}
	reply(w, upid)
}

func (f *fakePVE) vm(w http.ResponseWriter, r *http.Request, vm *fakeVM, what string) {
	switch {
	case what == "config" && r.Method == http.MethodGet:
		out := map[string]any{"digest": fmt.Sprintf("d%d", vm.version)}
		for k, v := range vm.config {
			out[k] = v
		}
		reply(w, out)
	case what == "config" && r.Method == http.MethodPut:
		r.ParseForm()
		if d := r.PostForm.Get("digest"); d != "" && d != fmt.Sprintf("d%d", vm.version) {
			fail(w, http.StatusInternalServerError, "configuration file has been changed by another user")
			return
		}
		for k := range r.PostForm {
			if k != "description" && k != "tags" && k != "digest" {
				f.t.Errorf("unexpected config change %s", k)
			}
		}
		if _, ok := r.PostForm["description"]; ok {
			vm.config["description"] = r.PostForm.Get("description")
		}
		if _, ok := r.PostForm["tags"]; ok {
			vm.config["tags"] = r.PostForm.Get("tags")
		}
		vm.version++
		reply(w, nil)
	case what == "status/current" && r.Method == http.MethodGet:
		reply(w, map[string]any{"status": vm.status, "lock": vm.lock})
	case what == "status/start" && r.Method == http.MethodPost:
		f.starts++
		vm.status = "running"
		reply(w, f.task(1, "qmstart", vm.vmid))
	case what == "resize" && r.Method == http.MethodPut:
		r.ParseForm()
		size := r.PostForm.Get("size")
		cur := strings.Split(vm.config["scsi0"], "size=")[1]
		if r.PostForm.Get("disk") != "scsi0" || !strings.HasSuffix(size, "G") || gib(size) < gib(cur) {
			fail(w, http.StatusInternalServerError, "shrinking disks is not supported")
			return
		}
		f.resizes++
		vm.config["scsi0"] = strings.Split(vm.config["scsi0"], "size=")[0] + "size=" + size
		vm.version++
		reply(w, f.task(1, "resize", vm.vmid))
	default:
		f.t.Errorf("unexpected %s on VM %d %s", r.Method, vm.vmid, what)
		fail(w, http.StatusNotImplemented, "not implemented by the fake")
	}
}

func gib(s string) float64 {
	n, _ := strconv.ParseFloat(s[:len(s)-1], 64)
	if s[len(s)-1] == 'M' {
		return n / 1024
	}
	return n
}
