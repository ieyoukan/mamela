package proxmox

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The permission catalog: every Proxmox API call Mamela makes, and every parameter it sends when
// creating or changing a VM, with the privileges Proxmox VE checks for it. It is the single source
// of the operating token's grants (see lab.Grants), and the client refuses to send any request or
// parameter that is not in it, so a new API call cannot be added without saying what it needs.
//
// Verified against Proxmox VE 9.2 (pve-manager 9.2.26 on the lab node): the API schema's
// "permissions" (/usr/share/pve-docs/api-viewer/apidoc.js) and the checks inside the handlers
// (PVE/API2/Qemu.pm create_vm, check_storage_access, check_vm_modify_config_perm,
// check_vm_create_serial_perm, check_bridge_access; PVE/Storage.pm check_volume_access;
// PVE/GuestHelpers.pm assert_tag_permissions; PVE/API2/Cluster.pm resources).

// Scope is where a privilege is checked, in terms of the lab.
type Scope string

const (
	ScopeVM           Scope = "vm"            // /vms/<vmid>: lab VMs get it through the lab pool's ACL
	ScopePool         Scope = "pool"          // /pool/<lab pool>
	ScopeDiskStorage  Scope = "disk-storage"  // /storage/<placement.disk_storage>
	ScopeImageStorage Scope = "image-storage" // /storage/<placement.image_storage>
	ScopeBridge       Scope = "bridge"        // /sdn/zones/<zone>/<bridge>
	ScopeNode         Scope = "node"          // /nodes/<lab node>
	ScopeRoot         Scope = "root"          // /
)

// Need is one permission check.
type Need struct {
	Scope Scope
	Privs []string
	// Any: one of Privs is enough (Proxmox's "any"). Mamela grants the first, the narrowest.
	Any  bool
	Why  string
	When string // the condition, if the check is not always made
}

// Grant is the privilege Mamela grants to satisfy the need: all of Privs, or the first with Any.
func (n Need) Grant() []string {
	if n.Any {
		return n.Privs[:1]
	}
	return n.Privs
}

// Token is which of Mamela's tokens makes a call.
type Token string

const (
	Operating Token = "operating" // changes the lab; its grants are derived from this catalog
	Audit     Token = "audit"     // read-only everywhere (PVEAuditor on /); preflight and postflight
)

// Endpoint is one API call.
type Endpoint struct {
	Method string
	Path   string // with {node}, {vmid}, {storage}, {upid}, {kind}
	Tokens []Token
	Needs  []Need
}

func vm(why string, privs ...string) Need { return Need{Scope: ScopeVM, Privs: privs, Why: why} }

// Endpoints is every API call Mamela makes.
var Endpoints = []Endpoint{
	{"GET", "/cluster/status", []Token{Audit}, []Need{{Scope: ScopeRoot, Privs: []string{"Sys.Audit"}, Why: "quorum and node states"}}},
	{"GET", "/cluster/resources", []Token{Operating, Audit}, []Need{
		// The call itself is open to every user; Proxmox lists only what the caller may see.
		vm("list the lab's VMs (only VMs with VM.Audit are listed)", "VM.Audit"),
		{Scope: ScopePool, Privs: []string{"Pool.Audit"}, Why: "be told which pool each VM is in (left out without Pool.Audit)"},
	}},
	{"GET", "/cluster/nextid", []Token{Operating}, nil}, // open to every user
	{"GET", "/access/permissions", []Token{Operating}, nil},
	{"GET", "/nodes/{node}/status", []Token{Audit}, []Need{{Scope: ScopeNode, Privs: []string{"Sys.Audit"}, Why: "memory headroom"}}},
	{"GET", "/nodes/{node}/storage", []Token{Operating}, []Need{{Scope: ScopeImageStorage, Privs: []string{"Datastore.Audit", "Datastore.AllocateSpace"},
		Any: true, Why: "see that the image storage accepts imports (only storages with Audit or AllocateSpace are listed)"}}},
	{"GET", "/nodes/{node}/storage/{storage}/status", []Token{Audit}, nil},
	{"GET", "/nodes/{node}/storage/{storage}/content", []Token{Operating}, []Need{{Scope: ScopeImageStorage,
		Privs: []string{"Datastore.Audit", "Datastore.AllocateSpace"}, Any: true, Why: "find uploaded images and seeds"}}},
	{"POST", "/nodes/{node}/storage/{storage}/upload", []Token{Operating}, []Need{{Scope: ScopeImageStorage,
		Privs: []string{"Datastore.AllocateTemplate"}, Why: "upload the pinned cloud image (content import) and seeds (content iso)"}}},
	// Task lists and states: open to every user for the user's own tasks (Sys.Audit on the node
	// only for other users' tasks, which Mamela does not read).
	{"GET", "/nodes/{node}/tasks", []Token{Operating}, nil},
	{"GET", "/nodes/{node}/tasks/{upid}/status", []Token{Operating}, nil},
	{"GET", "/nodes/{node}/{kind}/{vmid}/config", []Token{Audit}, nil},
	{"GET", "/nodes/{node}/qemu/{vmid}/config", []Token{Operating, Audit}, []Need{vm("read a lab VM's configuration and marker", "VM.Audit")}},
	{"GET", "/nodes/{node}/qemu/{vmid}/status/current", []Token{Operating}, []Need{vm("read a lab VM's state and lock", "VM.Audit")}},
	{"GET", "/nodes/{node}/qemu/{vmid}/agent/network-get-interfaces", []Token{Operating}, []Need{
		{Scope: ScopeVM, Privs: []string{"VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}, Any: true,
			Why: "read the addresses qemu-guest-agent reports (mamela lab tunnel)"}}},
	{"POST", "/nodes/{node}/qemu", []Token{Operating}, []Need{{Scope: ScopePool, Privs: []string{"VM.Allocate"},
		Why: "create a VM in the lab pool"}}}, // plus the needs of each parameter (CreateParams)
	{"PUT", "/nodes/{node}/qemu/{vmid}/config", []Token{Operating}, nil}, // the needs of each parameter (ConfigParams)
	{"PUT", "/nodes/{node}/qemu/{vmid}/resize", []Token{Operating}, []Need{
		vm("grow a lab VM's disk", "VM.Config.Disk"),
		{Scope: ScopeDiskStorage, Privs: []string{"Datastore.AllocateSpace"}, Why: "grow a disk on the disk storage"}}},
	{"POST", "/nodes/{node}/qemu/{vmid}/status/start", []Token{Operating}, []Need{vm("start a lab VM", "VM.PowerMgmt")}},
	{"POST", "/nodes/{node}/qemu/{vmid}/status/reboot", []Token{Operating}, []Need{vm("restart a lab VM to apply a new size", "VM.PowerMgmt")}},
	// mamela destroy only (a client from WithDestroy). Deleting uploaded files would need
	// Datastore.Allocate on the whole storage (which also holds other guests' backups and
	// templates), so Mamela never does it: a person removes them (mamela lab cleanup).
	{"POST", "/nodes/{node}/qemu/{vmid}/status/stop", []Token{Operating}, []Need{vm("stop a lab VM before destroying it (mamela destroy)", "VM.PowerMgmt")}},
	{"DELETE", "/nodes/{node}/qemu/{vmid}", []Token{Operating}, []Need{vm("destroy a lab VM and its disks (mamela destroy)", "VM.Allocate")}},
}

// CreateParams is every parameter Mamela may send to POST /nodes/{node}/qemu ("{n}" stands for
// the device number). Checked with the pool, so grants on the lab pool suffice.
var CreateParams = map[string][]Need{
	"vmid":        nil,
	"pool":        nil, // VM.Allocate on the pool (the endpoint's need)
	"name":        {vm("name", "VM.Config.Options")},
	"description": {vm("ownership marker", "VM.Config.Options")},
	"ostype":      {vm("guest OS type", "VM.Config.Options")},
	"onboot":      {vm("start on boot (off)", "VM.Config.Options")},
	"agent":       {vm("enable qemu-guest-agent", "VM.Config.Options")},
	"cores":       {vm("vCPUs", "VM.Config.CPU")},
	"memory":      {vm("memory", "VM.Config.Memory")},
	"scsihw":      {vm("SCSI controller", "VM.Config.HWType")},
	"vga":         {vm("display (serial)", "VM.Config.HWType")},
	"serial{n}":   {vm("serial console socket (check_vm_create_serial_perm)", "VM.Config.HWType")},
	"boot":        {vm("boot order", "VM.Config.Disk")},
	// Drives are not checked against VM.Config.Disk/CDROM at create; only their storage is.
	"scsi{n}": {
		{Scope: ScopeDiskStorage, Privs: []string{"Datastore.AllocateSpace"}, Why: "allocate the VM's disk"},
		{Scope: ScopeImageStorage, Privs: []string{"Datastore.Audit", "Datastore.AllocateSpace"}, Any: true,
			Why: "read the cloud image to import (check_volume_access on an import volume)"},
	},
	"ide{n}": {
		{Scope: ScopeImageStorage, Privs: []string{"Datastore.Audit", "Datastore.AllocateSpace"}, Any: true,
			Why: "attach the cloud-init seed ISO (check_volume_access on an iso volume)", When: "with a Control Plane (seed)"},
		// Proxmox's cloud-init drive ("<storage>:cloudinit") is not checked at create.
	},
	"net{n}": {
		vm("network device", "VM.Config.Network"),
		{Scope: ScopeBridge, Privs: []string{"SDN.Use"}, Why: "connect to the bridge (check_bridge_access)"},
	},
	"ciuser":  {vm("cloud-init user (Proxmox cloud-init drive)", "VM.Config.Cloudinit")},
	"sshkeys": {vm("cloud-init SSH keys (Proxmox cloud-init drive)", "VM.Config.Cloudinit")},
	"ipconfig{n}": {{Scope: ScopeVM, Privs: []string{"VM.Config.Cloudinit", "VM.Config.Network"}, Any: true,
		Why: "cloud-init network (Proxmox cloud-init drive)"}},
	// Never "tags": at create, Proxmox checks tags on /vms/<vmid> before the VM is in the pool,
	// so a pool-scoped token is refused (GuestHelpers.pm assert_tag_permissions).
}

// ConfigParams is every parameter Mamela may send to PUT /nodes/{node}/qemu/{vmid}/config.
var ConfigParams = map[string][]Need{
	"digest":      nil,
	"description": {vm("mark the VM ready", "VM.Config.Options")},
	"tags":        {vm("tag the VM (allowed once it is in the pool; tags are free-form here)", "VM.Config.Options")},
	"cores":       {vm("change the vCPUs", "VM.Config.CPU")},
	"memory":      {vm("change the memory", "VM.Config.Memory")},
}

var segment = regexp.MustCompile(`\{[a-z]+\}`)

// LookupEndpoint finds the catalog entry for a request (path without query). When several match,
// the most specific one (fewest placeholders) is used.
func LookupEndpoint(method, path string) (Endpoint, bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	got := strings.Split(path, "/")
	var best Endpoint
	found := false
	for _, e := range Endpoints {
		if e.Method != method {
			continue
		}
		want := strings.Split(e.Path, "/")
		if len(want) != len(got) {
			continue
		}
		match := true
		for i := range want {
			if segment.MatchString(want[i]) {
				if got[i] == "" {
					match = false
				}
				continue
			}
			if want[i] != got[i] {
				match = false
				break
			}
		}
		if match && (!found || strings.Count(e.Path, "{") < strings.Count(best.Path, "{")) {
			best, found = e, true
		}
	}
	return best, found
}

var digits = regexp.MustCompile(`[0-9]+$`)

// ParamKey maps a parameter to its catalog key ("net0" -> "net{n}").
func ParamKey(params map[string][]Need, name string) (string, bool) {
	if _, ok := params[name]; ok {
		return name, true
	}
	if k := digits.ReplaceAllString(name, "{n}"); k != name {
		if _, ok := params[k]; ok {
			return k, true
		}
	}
	return "", false
}

// checkParams refuses parameters that are not in the catalog.
func checkParams(params map[string][]Need, names []string, where string) error {
	var unknown []string
	for _, n := range names {
		if _, ok := ParamKey(params, n); !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("proxmox: %s: parameter(s) %s are not in Mamela's permission catalog "+
			"(internal/proxmox/permissions.go); add them with the privileges Proxmox checks before sending them: %w",
			where, strings.Join(unknown, ", "), ErrNotSent)
	}
	return nil
}

// Call is one API call an operation makes, with every parameter it sends (form, multipart or
// query; none if Params is empty) and the condition under which it is made, if any.
type Call struct {
	Method string
	Path   string
	Params []string
	When   string
	Maybe  bool // made only in some cases (When says which)
}

// NeedsFor lists what a sequence of calls needs, sorted by scope and privilege.
func NeedsFor(calls []Call) []Requirement {
	acc := map[[2]string][]string{}
	always := map[[2]string]bool{}
	maybe := false
	add := func(where, when string, n Need) {
		for _, p := range n.Grant() {
			k := [2]string{string(n.Scope), p}
			always[k] = always[k] || !maybe
			reason := where + ": " + n.Why
			for _, w := range []string{n.When, when} {
				if w != "" {
					reason += " (" + w + ")"
				}
			}
			if !slices.Contains(acc[k], reason) {
				acc[k] = append(acc[k], reason)
			}
		}
	}
	for _, c := range calls {
		e, ok := LookupEndpoint(c.Method, strings.NewReplacer("{node}", "n", "{vmid}", "1", "{storage}", "s", "{upid}", "u").Replace(c.Path))
		if !ok {
			panic("proxmox: call not in the catalog: " + c.Method + " " + c.Path)
		}
		where := c.Method + " " + c.Path
		maybe = c.Maybe
		for _, n := range e.Needs {
			add(where, c.When, n)
		}
		var params map[string][]Need // the calls whose parameters Proxmox checks one by one
		switch {
		case c.Method == "POST" && c.Path == "/nodes/{node}/qemu":
			params = CreateParams
		case c.Method == "PUT" && c.Path == "/nodes/{node}/qemu/{vmid}/config":
			params = ConfigParams
		}
		if params == nil {
			continue // other calls' parameters need nothing beyond the call itself
		}
		for _, name := range c.Params {
			k, ok := ParamKey(params, name)
			if !ok {
				panic("proxmox: parameter not in the catalog: " + where + " " + name)
			}
			for _, n := range params[k] {
				add(where+" "+name, c.When, n)
			}
		}
	}
	out := sortRequirements(acc)
	for i := range out {
		out[i].Always = always[[2]string{string(out[i].Scope), out[i].Priv}]
	}
	return out
}

func sortRequirements(acc map[[2]string][]string) []Requirement {
	var out []Requirement
	for k, reasons := range acc {
		slices.Sort(reasons)
		out = append(out, Requirement{Scope: Scope(k[0]), Priv: k[1], Reasons: reasons, Always: true})
	}
	slices.SortFunc(out, func(a, b Requirement) int {
		if c := strings.Compare(string(a.Scope), string(b.Scope)); c != 0 {
			return c
		}
		return strings.Compare(a.Priv, b.Priv)
	})
	return out
}

// Requirement is a privilege the operating token needs, with every reason for it.
type Requirement struct {
	Scope   Scope
	Priv    string
	Reasons []string
	Always  bool // false: only some calls that may not happen need it (NeedsFor)
}

// Required lists what the operating token needs for the given create parameters (the ones the
// provider sends; they depend on the site) plus every operating-token endpoint and config
// parameter, sorted by scope and privilege.
func Required(createParams []string) []Requirement {
	acc := map[[2]string][]string{}
	add := func(where string, n Need) {
		for _, p := range n.Grant() {
			k := [2]string{string(n.Scope), p}
			reason := where + ": " + n.Why
			if n.When != "" {
				reason += " (" + n.When + ")"
			}
			if !slices.Contains(acc[k], reason) {
				acc[k] = append(acc[k], reason)
			}
		}
	}
	for _, e := range Endpoints {
		if !slices.Contains(e.Tokens, Operating) {
			continue
		}
		for _, n := range e.Needs {
			add(e.Method+" "+e.Path, n)
		}
	}
	for _, name := range createParams {
		k, ok := ParamKey(CreateParams, name)
		if !ok {
			continue // refused by the client anyway
		}
		for _, n := range CreateParams[k] {
			add("POST /nodes/{node}/qemu "+name, n)
		}
	}
	for name, needs := range ConfigParams {
		for _, n := range needs {
			add("PUT /nodes/{node}/qemu/{vmid}/config "+name, n)
		}
	}
	return sortRequirements(acc)
}

var permFailed = regexp.MustCompile(`Permission check failed \(([^,]+), ([A-Za-z.]+)\)`)

// ExplainPermission adds what Mamela knows to a "Permission check failed (path, priv)" message:
// whether the privilege is in the catalog (then the token lacks it) or not (then the catalog is
// incomplete, a bug in Mamela). It returns "" when msg is not a permission failure.
func ExplainPermission(msg string) string {
	m := permFailed.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	path, priv := m[1], m[2]
	var reasons []string
	for _, r := range Required(slices.Collect(func(yield func(string) bool) {
		for k := range CreateParams {
			if !yield(strings.ReplaceAll(k, "{n}", "0")) {
				return
			}
		}
	})) {
		if r.Priv == priv && scopeMatches(r.Scope, path) {
			reasons = append(reasons, r.Reasons...)
		}
	}
	if len(reasons) == 0 {
		return fmt.Sprintf("Proxmox requires %s on %s, which is not in Mamela's permission catalog: this is a bug in Mamela "+
			"(add it to internal/proxmox/permissions.go, then run the bootstrap script again)", priv, path)
	}
	return fmt.Sprintf("the operating token lacks %s on %s, which Mamela needs for %s; run `mamela lab permissions` to see "+
		"what is missing, then the bootstrap script again", priv, path, strings.Join(reasons, "; "))
}

func scopeMatches(s Scope, path string) bool {
	switch s {
	case ScopeVM:
		return strings.HasPrefix(path, "/vms/") || strings.HasPrefix(path, "/pool/")
	case ScopePool:
		return strings.HasPrefix(path, "/pool/")
	case ScopeDiskStorage, ScopeImageStorage:
		return strings.HasPrefix(path, "/storage/")
	case ScopeBridge:
		return strings.HasPrefix(path, "/sdn/")
	case ScopeNode:
		return strings.HasPrefix(path, "/nodes/")
	case ScopeRoot:
		return path == "/"
	}
	return false
}
