// Package fakepve is a small fake of the Proxmox VE API calls the example provider uses, for its
// tests: VMs in memory, tasks that end at once, and configuration digests that refuse a
// configuration change based on an older read (HTTP 500, as Proxmox does).
package fakepve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
)

// VM is one fake VM.
type VM struct {
	VMID        int64
	Name        string
	Node        string
	Pool        string
	Description string
	Cores       int64
	Memory      int64
	Serial      int // bumped on every change; the digest is derived from it
}

func (v VM) digest() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%d", v.VMID, v.Cores, v.Memory, v.Serial)))
	return hex.EncodeToString(sum[:20])
}

// Server is the fake API.
type Server struct {
	mu  sync.Mutex
	vms map[int64]*VM
}

// New returns a fake with no VMs.
func New() *Server { return &Server{vms: map[int64]*VM{}} }

// Put stores a VM as is.
func (s *Server) Put(v VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[v.VMID] = &v
}

// VMs returns a copy of the VMs.
func (s *Server) VMs() map[int64]VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]VM{}
	for k, v := range s.vms {
		out[k] = *v
	}
	return out
}

func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, map[string]any{"data": data}, json.Deterministic(true))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		ids := make([]int64, 0, len(s.vms))
		for id := range s.vms {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			v := s.vms[id]
			out = append(out, map[string]any{"vmid": v.VMID, "name": v.Name, "node": v.Node, "pool": v.Pool, "type": "qemu", "status": "stopped"})
		}
		reply(w, 200, out)
	})
	vm := func(w http.ResponseWriter, r *http.Request) *VM {
		id, _ := strconv.ParseInt(r.PathValue("vmid"), 10, 64)
		v, ok := s.vms[id]
		if !ok || v.Node != r.PathValue("node") {
			reply(w, 500, nil)
			return nil
		}
		return v
	}
	mux.HandleFunc("GET /nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			reply(w, 200, map[string]any{"name": v.Name, "cores": v.Cores, "memory": strconv.FormatInt(v.Memory, 10), "description": v.Description + "\n", "digest": v.digest()})
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, _ := strconv.ParseInt(r.PostForm.Get("vmid"), 10, 64)
		if _, exists := s.vms[id]; exists || id == 0 {
			reply(w, 500, nil) // "VM already exists"
			return
		}
		cores, _ := strconv.ParseInt(r.PostForm.Get("cores"), 10, 64)
		mem, _ := strconv.ParseInt(r.PostForm.Get("memory"), 10, 64)
		s.vms[id] = &VM{VMID: id, Name: r.PostForm.Get("name"), Node: r.PathValue("node"), Pool: r.PostForm.Get("pool"),
			Description: r.PostForm.Get("description"), Cores: cores, Memory: mem}
		reply(w, 200, fmt.Sprintf("UPID:%s:0000AAAA:0000BBBB:00000000:qmcreate:%d:fake@pve!t:", r.PathValue("node"), id))
	})
	mux.HandleFunc("GET /nodes/{node}/tasks/{upid}/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"status": "stopped", "exitstatus": "OK"})
	})
	mux.HandleFunc("PUT /nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		v := vm(w, r)
		if v == nil {
			return
		}
		r.ParseForm()
		if d := r.PostForm.Get("digest"); d != "" && d != v.digest() {
			reply(w, 500, nil) // "detected modified configuration"
			return
		}
		if c := r.PostForm.Get("cores"); c != "" {
			v.Cores, _ = strconv.ParseInt(c, 10, 64)
		}
		if m := r.PostForm.Get("memory"); m != "" {
			v.Memory, _ = strconv.ParseInt(m, 10, 64)
		}
		v.Serial++
		reply(w, 200, nil)
	})
	mux.ServeHTTP(w, r)
}
