package lab

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func exampleSite(t *testing.T) *Site {
	t.Helper()
	s, err := LoadSite(filepath.Join("..", "..", "lab", "site.example.toml"))
	if err != nil {
		t.Fatalf("the example site must be valid: %v", err)
	}
	return s
}

func TestLoadSiteRejectsUnknownKeysAndWeakSettings(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "lab", "site.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(string) string{
		"unknown key": func(s string) string { return s + "\n[extra]\nx = 1\n" },
		"root token":  func(s string) string { return strings.Replace(s, `"mamela@pve!lab"`, `"root@pam!lab"`, 1) },
		"same tokens": func(s string) string { return strings.Replace(s, `"mamela-audit@pve!audit"`, `"mamela@pve!lab"`, 1) },
		"same user":   func(s string) string { return strings.Replace(s, `"mamela-audit@pve!audit"`, `"mamela@pve!audit"`, 1) },
		"http":        func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		"no tls pin": func(s string) string {
			return strings.Replace(s, `tls_fingerprint = "AA`, `tls_fingerprint_x = "AA`, 1)
		},
		"huge vmid range": func(s string) string { return strings.Replace(s, "vmid_max = 7099", "vmid_max = 9999", 1) },
		"thinpool 100%": func(s string) string {
			return strings.Replace(s, "max_thinpool_used_percent = 80", "max_thinpool_used_percent = 100", 1)
		},
		"tiny mem reserve": func(s string) string {
			return strings.Replace(s, "min_host_mem_available_mib = 2048", "min_host_mem_available_mib = 100", 1)
		},
	} {
		path := filepath.Join(t.TempDir(), "site.toml")
		os.WriteFile(path, []byte(edit(string(example))), 0o600)
		if _, err := LoadSite(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The script adds, and removes only ACL entries of Mamela's own operating user and token.
func TestBootstrapScriptRemovesOnlyMamelasOwnACLs(t *testing.T) {
	s := exampleSite(t)
	script := s.BootstrapScript()
	allowedDeletes := []string{
		`  if [ "$type" = token ]; then pveum acl delete "$path" --tokens "$ugid" --roles "$role"; else pveum acl delete "$path" --users "$ugid" --roles "$role"; fi`,
	}
	var commands []string // the script without comment lines and the allowed deletions
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && !slices.Contains(allowedDeletes, line) {
			commands = append(commands, line)
		}
	}
	code := strings.Join(commands, "\n")
	if !strings.Contains(script, `"$keep" 'mamela@pve' 'mamela@pve!lab' |`) {
		t.Error("the removal must look only at the operating user and token")
	}
	for _, forbidden := range []string{" delete", " destroy", "qm ", "pct ", "acl modify /vms", "--roles Administrator", "--roles PVEAdmin", "root@pam"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("bootstrap script contains %q", forbidden)
		}
	}
	for _, g := range s.Grants() {
		for _, want := range []string{
			"pveum acl modify " + g.Path + " --users mamela@pve --roles " + g.Role,
			"pveum acl modify " + g.Path + " --tokens 'mamela@pve!lab' --roles " + g.Role,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("bootstrap script lacks %q", want)
			}
		}
	}
	if !strings.Contains(script, "pveum acl modify / --tokens 'mamela-audit@pve!audit' --roles PVEAuditor") {
		t.Error("audit token is not read-only PVEAuditor on /")
	}
	if !strings.Contains(script, `pvesm set local --content "$content,import"`) || strings.Contains(code, "pvesm set local-lvm") {
		t.Error("the script must add the import content type to the image storage only, keeping the others")
	}
	if strings.Count(script, "acl modify / ") != 2 {
		t.Error("only the audit user and token may have ACLs on /")
	}
}

func TestExcessPermissions(t *testing.T) {
	s := exampleSite(t)
	ok := map[string]map[string]int{
		"/pool/mamela-lab":              {"VM.Allocate": 1, "VM.PowerMgmt": 1},
		"/vms/7000":                     {"VM.Config.CPU": 1},
		"/storage/local-lvm":            {"Datastore.AllocateSpace": 1},
		"/sdn/zones/localnetwork/vmbr0": {"SDN.Use": 1},
		"/":                             {"Sys.Audit": 0},
	}
	if ex := s.ExcessPermissions(ok); len(ex) != 0 {
		t.Errorf("flagged allowed permissions: %v", ex)
	}
	for name, perms := range map[string]map[string]map[string]int{
		"vm outside range": {"/vms/105": {"VM.PowerMgmt": 1}},
		"root path":        {"/": {"Sys.Audit": 1}},
		"all vms":          {"/vms": {"VM.Audit": 1}},
		"wrong priv":       {"/storage/local-lvm": {"Datastore.Allocate": 1}},
		"user admin":       {"/access": {"User.Modify": 1}},
		"other pool":       {"/pool/production": {"VM.Allocate": 1}},
		// Granted by older bootstraps, needed by no API call Mamela makes.
		"node audit":      {"/nodes/pve-node-1": {"Sys.Audit": 1}},
		"cdrom":           {"/pool/mamela-lab": {"VM.Config.CDROM": 1}},
		"disk store read": {"/storage/local-lvm": {"Datastore.Audit": 1}},
	} {
		if ex := s.ExcessPermissions(perms); len(ex) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
}

// grantedPermissions is what /access/permissions shows for a token set up by the bootstrap.
func grantedPermissions() map[string]map[string]int {
	s, err := LoadSite(filepath.Join("..", "..", "lab", "site.example.toml"))
	if err != nil {
		panic(err)
	}
	out := map[string]map[string]int{}
	for _, g := range s.Grants() {
		out[g.Path] = map[string]int{}
		for _, p := range g.Privs {
			out[g.Path][p] = 1
		}
	}
	return out
}

func healthyFacts() *Facts {
	return &Facts{
		Quorate: true, NodesTotal: 3, NodesOnline: 3, NodeOnline: true,
		MemAvailable: 8 << 30, DiskTotal: 100 << 30, DiskUsed: 20 << 30, ImportStorages: []string{"local"},
		LabPermissions: grantedPermissions(),
		Guests: []Guest{
			{VMID: 101, Type: "qemu", Node: "pve-node-1", Name: "prod-db", Status: "running", Uptime: 1000, ConfigDigest: "d1"},
			{VMID: 102, Type: "lxc", Node: "pve-node-2", Name: "dns", Status: "running", Uptime: 2000, ConfigDigest: "d2"},
		},
	}
}

func codes(fs []Finding, sev Severity) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == sev {
			out = append(out, f.Code)
		}
	}
	return out
}

func TestCheckHealthy(t *testing.T) {
	fs := Check(exampleSite(t), healthyFacts())
	if Blocked(fs) || len(codes(fs, Warn)) != 0 {
		t.Fatalf("healthy facts: %+v", fs)
	}
}

func TestCheckBlocks(t *testing.T) {
	s := exampleSite(t)
	tests := map[string]struct {
		edit func(*Facts)
		code string
		sev  Severity
	}{
		"no quorum":            {func(f *Facts) { f.Quorate = false }, "LAB-0001", Block},
		"no quorum margin":     {func(f *Facts) { f.NodesTotal, f.NodesOnline = 4, 3 }, "LAB-0002", Warn},
		"node offline":         {func(f *Facts) { f.NodeOnline = false }, "LAB-0003", Block},
		"broad token":          {func(f *Facts) { f.LabPermissions["/"] = map[string]int{"VM.Allocate": 1} }, "LAB-0101", Block},
		"bootstrap not re-run": {func(f *Facts) { delete(f.LabPermissions["/pool/mamela-lab"], "Pool.Audit") }, "LAB-0104", Block},
		// The operating token is not shown pools; membership comes from the audit token's view.
		"lab VM, pool hidden from token": {func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 7000, Pool: "mamela-lab", Tags: "mamela-lab", Name: "lab-1"})
			f.LabVisibleGuests = []Guest{{VMID: 7000, Name: "lab-1"}}
		}, "LAB-0102", OK},
		"token sees others": {func(f *Facts) { f.LabVisibleGuests = []Guest{{VMID: 101, Name: "prod-db"}} }, "LAB-0102", Block},
		"low memory":        {func(f *Facts) { f.MemAvailable = 2500 << 20 }, "LAB-0201", Block},
		"thin pool full":    {func(f *Facts) { f.DiskUsed = 75 << 30 }, "LAB-0202", Block},
		"squatter in range": {func(f *Facts) { f.Guests = append(f.Guests, Guest{VMID: 7005, Name: "someone"}) }, "LAB-0301", Block},
		"pool outside range": {func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 150, Pool: "mamela-lab", Tags: "mamela-lab", Name: "x"})
		}, "LAB-0302", Block},
		// A VM whose create has not been finished has no tag yet; it is a lab guest and counts.
		"untagged in pool counts": {func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 7001, Pool: "mamela-lab", Name: "x"})
		}, "LAB-0303", Block},
		"budget full": {func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 7000, Pool: "mamela-lab", Tags: "mamela-lab", Name: "lab-1"})
		}, "LAB-0303", Block},
	}
	for name, tt := range tests {
		f := healthyFacts()
		tt.edit(f)
		fs := Check(s, f)
		if got := codes(fs, tt.sev); !contains(got, tt.code) {
			t.Errorf("%s: want %s %s, got %+v", name, tt.sev, tt.code, fs)
		}
	}
}

func TestCompare(t *testing.T) {
	s := exampleSite(t)
	before := NewBaseline(s, healthyFacts())
	b2, err := DecodeBaseline(before.Encode())
	if err != nil || len(b2.Guests) != 2 {
		t.Fatalf("round trip: %v %+v", err, b2)
	}
	after := healthyFacts()
	after.Guests[0].Uptime, after.Guests[1].Uptime = 1100, 2100
	after.Guests = append(after.Guests, Guest{VMID: 7000, Pool: "mamela-lab", Tags: "mamela-lab", Name: "lab-1", Status: "running"})
	if fs := Compare(s, before, after); Blocked(fs) || len(codes(fs, Warn)) != 0 {
		t.Fatalf("unchanged world: %+v", fs)
	}
	for name, tt := range map[string]struct {
		edit func(*Facts)
		code string
	}{
		"disappeared":         {func(f *Facts) { f.Guests = f.Guests[1:] }, "LAB-0401"},
		"stopped":             {func(f *Facts) { f.Guests[0].Status = "stopped" }, "LAB-0402"},
		"reconfigured":        {func(f *Facts) { f.Guests[0].ConfigDigest = "other" }, "LAB-0403"},
		"restarted":           {func(f *Facts) { f.Guests[0].Uptime = 5 }, "LAB-0404"},
		"new guest":           {func(f *Facts) { f.Guests = append(f.Guests, Guest{VMID: 150, Name: "new"}) }, "LAB-0405"},
		"create not finished": {func(f *Facts) { f.Guests = append(f.Guests, Guest{VMID: 7003, Name: "web"}) }, "LAB-0405"},
	} {
		f := healthyFacts()
		f.Guests[0].Uptime, f.Guests[1].Uptime = 1100, 2100
		tt.edit(f)
		fs := Compare(s, before, f)
		found := false
		for _, x := range fs {
			found = found || x.Code == tt.code
		}
		if !found {
			t.Errorf("%s: want %s, got %+v", name, tt.code, fs)
		}
	}
}

func TestReadSecretRequiresPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(path, []byte("abc\n"), 0o644)
	if _, err := ReadSecret(path); err == nil {
		t.Error("accepted a world-readable secret file")
	}
	os.Chmod(path, 0o600)
	if s, err := ReadSecret(path); err != nil || s != "abc" {
		t.Errorf("ReadSecret: %q %v", s, err)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestImageStorageMustAcceptImports(t *testing.T) {
	f := healthyFacts()
	f.ImportStorages = []string{"other"}
	if got := codes(Check(exampleSite(t), f), Block); len(got) != 1 || got[0] != "LAB-0203" {
		t.Fatalf("blocks %v, want only LAB-0203", got)
	}
}

func TestCheckForExistingVMsOnly(t *testing.T) {
	s := exampleSite(t)
	f := healthyFacts()
	f.Guests = append(f.Guests, Guest{VMID: 7000, Type: "qemu", Node: "pve-node-1", Name: "web", Pool: "mamela-lab", Tags: "mamela-lab"})
	if got := codes(CheckFor(s, f, 1), Block); len(got) != 1 || got[0] != "LAB-0303" {
		t.Fatalf("one more VM: blocks %v, want LAB-0303", got)
	}
	if got := codes(CheckFor(s, f, 0), Block); len(got) != 0 {
		t.Fatalf("no new VM: blocks %v", got)
	}
}

// The grants are derived from the permission catalog. This pins the result for the example site
// (with a Control Plane, so VMs get a mamelet seed instead of Proxmox's cloud-init drive): a
// change here is a change of the token's power and must be reviewed.
func TestGrantsFromCatalog(t *testing.T) {
	s := exampleSite(t)
	got := map[string]string{}
	for _, g := range s.Grants() {
		got[g.Path] = g.Role + " " + strings.Join(g.Privs, ",")
		for _, p := range g.Privs {
			if len(g.Reasons[p]) == 0 {
				t.Errorf("%s %s has no reason", g.Path, p)
			}
		}
	}
	want := map[string]string{
		"/pool/mamela-lab": "MamelaLabVM Pool.Audit,VM.Allocate,VM.Audit,VM.Config.CPU,VM.Config.Disk,VM.Config.HWType," +
			"VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.GuestAgent.Audit,VM.PowerMgmt",
		"/storage/local-lvm":            "MamelaLabDisk Datastore.AllocateSpace",
		"/storage/local":                "MamelaLabImage Datastore.AllocateTemplate,Datastore.Audit",
		"/sdn/zones/localnetwork/vmbr0": "MamelaLabNet SDN.Use",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("grants:\n got  %v\n want %v", got, want)
	}
	if s.Grants()[0].Path != "/pool/mamela-lab" {
		t.Error("the pool's grant must come first")
	}

	// Without a Control Plane, VMs use Proxmox's cloud-init drive, which needs VM.Config.Cloudinit.
	s.ControlPlane = ControlPlane{}
	if g := s.Grants()[0]; !slices.Contains(g.Privs, "VM.Config.Cloudinit") {
		t.Errorf("without a seed: %v", g.Privs)
	}
}

// Runs the script's filter of stale ACL entries on a sample of what Proxmox returns.
func TestBootstrapStaleACLFilter(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	s := exampleSite(t)
	var keep []string
	for _, g := range s.Grants() {
		keep = append(keep, g.Path+" "+g.Role)
	}
	acl := `[
{"path":"/pool/mamela-lab","roleid":"MamelaLabVM","type":"token","ugid":"mamela@pve!lab"},
{"path":"/nodes/pve-node-1","roleid":"MamelaLabAudit","type":"token","ugid":"mamela@pve!lab"},
{"path":"/storage/local","roleid":"MamelaLabISO","type":"user","ugid":"mamela@pve"},
{"path":"/","roleid":"PVEAuditor","type":"user","ugid":"mamela-audit@pve"},
{"path":"/","roleid":"Administrator","type":"user","ugid":"admin@pam"},
{"path":"/pool/mamela-lab","roleid":"MamelaLabVM","type":"user","ugid":"someone@pve"}]`
	cmd := exec.Command("python3", "-c", staleACLs, strings.Join(keep, "\\n"), "mamela@pve", "mamela@pve!lab")
	cmd.Stdin = strings.NewReader(acl)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "token mamela@pve!lab /nodes/pve-node-1 MamelaLabAudit\nuser mamela@pve /storage/local MamelaLabISO\n"
	if string(out) != want {
		t.Fatalf("stale entries:\n%s\nwant:\n%s", out, want)
	}
}

// The Control Plane listens off loopback only when mamelet reaches it directly (ADR 0003).
func TestControlPlaneListen(t *testing.T) {
	s, err := LoadSite("../../lab/site.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		url, listen, moveTo, host string
		ok                        bool
	}{
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "", "", true},
		{"https://127.0.0.1:8443", "0.0.0.0:8443", "", "", false}, // tunnel mode, exposed for nothing
		{"https://10.0.0.45:8443", "0.0.0.0:8443", "", "mamela-cp", true},
		{"https://10.0.0.45:8443", "10.0.0.45:8443", "", "", true},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "https://10.0.0.45:8443", "", true},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "http://10.0.0.45:8443", "", false},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "https://10.0.0.45", "", false},
		{"https://10.0.0.45:8443", "0.0.0.0:8443", "", "bad name!", false},
	} {
		v := *s
		v.ControlPlane.URL, v.ControlPlane.Listen, v.ControlPlane.MoveTo, v.ControlPlane.Host = c.url, c.listen, c.moveTo, c.host
		if err := v.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
	v := *s
	v.ControlPlane.MoveTo = "https://10.0.0.45:8443"
	if v.ControlPlane.Advertised() != "https://10.0.0.45:8443" || s.ControlPlane.Advertised() != s.ControlPlane.URL {
		t.Fatal("advertised URL")
	}
}
