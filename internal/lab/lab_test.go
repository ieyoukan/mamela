package lab

import (
	"os"
	"path/filepath"
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

func TestBootstrapScriptOnlyAdds(t *testing.T) {
	s := exampleSite(t)
	script := s.BootstrapScript()
	var commands []string // the script without comment lines
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			commands = append(commands, line)
		}
	}
	code := strings.Join(commands, "\n")
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
		"/nodes/pve-node-1":             {"Sys.Audit": 1},
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
	} {
		if ex := s.ExcessPermissions(perms); len(ex) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
}

func healthyFacts() *Facts {
	return &Facts{
		Quorate: true, NodesTotal: 3, NodesOnline: 3, NodeOnline: true,
		MemAvailable: 8 << 30, DiskTotal: 100 << 30, DiskUsed: 20 << 30, ImportStorages: []string{"local"},
		LabPermissions: map[string]map[string]int{"/pool/mamela-lab": {"VM.Allocate": 1}},
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
		"no quorum":         {func(f *Facts) { f.Quorate = false }, "LAB-0001", Block},
		"no quorum margin":  {func(f *Facts) { f.NodesTotal, f.NodesOnline = 4, 3 }, "LAB-0002", Warn},
		"node offline":      {func(f *Facts) { f.NodeOnline = false }, "LAB-0003", Block},
		"broad token":       {func(f *Facts) { f.LabPermissions["/"] = map[string]int{"VM.Allocate": 1} }, "LAB-0101", Block},
		"token sees others": {func(f *Facts) { f.LabVisibleGuests = []Guest{{VMID: 101, Name: "prod-db"}} }, "LAB-0102", Block},
		"low memory":        {func(f *Facts) { f.MemAvailable = 2500 << 20 }, "LAB-0201", Block},
		"thin pool full":    {func(f *Facts) { f.DiskUsed = 75 << 30 }, "LAB-0202", Block},
		"squatter in range": {func(f *Facts) { f.Guests = append(f.Guests, Guest{VMID: 7005, Name: "someone"}) }, "LAB-0301", Block},
		"untagged in pool": {func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 7001, Pool: "mamela-lab", Name: "x"})
		}, "LAB-0301", Block},
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
		"disappeared":  {func(f *Facts) { f.Guests = f.Guests[1:] }, "LAB-0401"},
		"stopped":      {func(f *Facts) { f.Guests[0].Status = "stopped" }, "LAB-0402"},
		"reconfigured": {func(f *Facts) { f.Guests[0].ConfigDigest = "other" }, "LAB-0403"},
		"restarted":    {func(f *Facts) { f.Guests[0].Uptime = 5 }, "LAB-0404"},
		"new guest":    {func(f *Facts) { f.Guests = append(f.Guests, Guest{VMID: 150, Name: "new"}) }, "LAB-0405"},
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
