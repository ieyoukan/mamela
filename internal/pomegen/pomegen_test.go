package pomegen

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

func op(o protocol.Op) protocol.Operation {
	return protocol.Operation{Op: o, Support: protocol.Unsupported, Why: "test"}
}

// A manifest with every kind of field: references (direct, listed, optional, inside objects),
// enums, objects, optional values, a node kind.
func rich() protocol.Manifest {
	str := protocol.Field{Type: protocol.FieldString}
	return protocol.Manifest{
		Schema: protocol.Schema, Provider: "cloud", Version: "0.1.0", API: "cloud/v1",
		Kinds: []protocol.Kind{
			{Kind: "CloudNetwork", Doc: "A network.", Operations: []protocol.Operation{op("create")},
				Spec: []protocol.Field{{Name: "cidr", Type: protocol.FieldString, Doc: "its addresses"}}},
			{Kind: "CloudSecurityGroup", Operations: []protocol.Operation{op("create")},
				Spec: []protocol.Field{
					{Name: "network", Type: protocol.FieldRef, Ref: "CloudNetwork"},
					{Name: "rules", Type: protocol.FieldList, Optional: true, Elem: &protocol.Field{Type: protocol.FieldObject, TypeName: "rule", Fields: []protocol.Field{
						{Name: "port", Type: protocol.FieldInt},
						{Name: "protocol", Type: protocol.FieldString, Values: []string{"tcp", "udp"}},
						{Name: "fromNetwork", Type: protocol.FieldRef, Ref: "CloudNetwork", Optional: true},
					}}},
				}},
			{Kind: "CloudVM", Node: true, Operations: []protocol.Operation{op("create")},
				Spec: []protocol.Field{
					{Name: "network", Type: protocol.FieldRef, Ref: "CloudNetwork"},
					{Name: "groups", Type: protocol.FieldList, Elem: &protocol.Field{Type: protocol.FieldRef, Ref: "CloudSecurityGroup"}},
					{Name: "size", Type: protocol.FieldString, Values: []string{"small", "large-2x"}, Doc: "how big"},
					{Name: "cpus", Type: protocol.FieldInt, Optional: true},
					{Name: "public", Type: protocol.FieldBool},
					{Name: "tags", Type: protocol.FieldList, Optional: true, Elem: &str},
					{Name: "type", Type: protocol.FieldString, Optional: true},
				}},
			{Kind: "CloudNoSpec", Operations: []protocol.Operation{op("create")}},
		},
	}
}

func TestGenerate(t *testing.T) {
	src, err := Generate(rich(), Options{Module: "Cloud"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Generate(rich(), Options{Module: "Cloud"})
	if !bytes.Equal(src, again) {
		t.Fatal("not deterministic")
	}
	for _, want := range []string{
		"pub type CloudVm(", "  network: CloudNetwork,", "  groups: List[CloudSecurityGroup],", "  size: CloudVmSize,",
		"  cpus: Option[Int],", "  tags: List[String],", "  type_: Option[String],",
		"pub fn cloud_vm(graph: Mamela.Graph, r: CloudVm) -> Mamela.Graph {",
		"pub fn cloud_vm_node(r: CloudVm) -> Mamela.Node {",
		"pub enum CloudVmSize {", "  Large2x", `Large2x -> Text("large-2x")`,
		"pub type Rule(", "  from_network: Option[CloudNetwork],",
		`Mamela.provided(graph, PROVIDER, "CloudNetwork", r.name, spec)`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("no %q in:\n%s", want, src)
		}
	}
	if strings.Contains(string(src), "CloudNoSpec") {
		t.Error("a kind without a spec is generated")
	}
	check(t, src)
}

// check compiles the source as a Pome's lib.emel with the repository's toolchain, offline: what is
// generated must be valid Emela. Skipped without the toolchain (scripts/install-toolchain.sh).
func check(t *testing.T, src []byte) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	emela := filepath.Join(root, ".tools/bin/emela")
	if _, err := os.Stat(emela); err != nil {
		t.Skip("no toolchain: run scripts/install-toolchain.sh")
	}
	dir := t.TempDir()
	toml := `[pome]
name = "example.com/cloud"
version = "0.1.0"
emela = "0.27.0"
module = "Cloud"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.3.0"
"github.com/emela-lang/x/dynamic" = "v0.5.1"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "` + filepath.Join(root, "pome/mamela") + `" }
`
	lock, err := os.ReadFile(filepath.Join(root, "examples/kv/pome/Pome.lock"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	for name, data := range map[string][]byte{"Pome.toml": []byte(toml), "Pome.lock": lock, "src/lib.emel": src} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(emela, "check", dir)
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "EMELA_POMEPROXY=off", "EMELA_POMESUMDB=off",
		"EMELA_POME_CACHE=" + filepath.Join(root, ".tools/pomes")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated Pome does not compile: %v\n%s\n%s", err, out, numbered(src))
	}
}

func numbered(src []byte) string {
	var b strings.Builder
	for i, l := range strings.Split(string(src), "\n") {
		fmt.Fprintf(&b, "%4d\t%s\n", i+1, l)
	}
	return b.String()
}

func TestRefused(t *testing.T) {
	m := rich()
	m.Kinds[1].Spec[0].Ref = "CloudNoSpec"
	if _, err := Generate(m, Options{}); err == nil || !strings.Contains(err.Error(), "CloudNoSpec") {
		t.Errorf("a reference to a kind without a spec: %v", err)
	}
	m = rich()
	m.Kinds[2].Spec[2].Values = []string{"a-b", "a_b"}
	if _, err := Generate(m, Options{}); err == nil || !strings.Contains(err.Error(), "same variant") {
		t.Errorf("values that make one variant: %v", err)
	}
	m = rich()
	for i := range m.Kinds {
		m.Kinds[i].Spec = nil
	}
	if _, err := Generate(m, Options{}); err == nil {
		t.Error("nothing to generate is not refused")
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string][2]string{
		"KVItem": {"KvItem", "kv_item"}, "AwsIamRole": {"AwsIamRole", "aws_iam_role"}, "retentionDays": {"RetentionDays", "retention_days"},
		"ecs-tasks": {"EcsTasks", "ecs_tasks"}, "large-2x": {"Large2x", "large_2x"}, "ProxmoxVM": {"ProxmoxVm", "proxmox_vm"},
	} {
		if u, s := upper(in), snake(in); u != want[0] || s != want[1] {
			t.Errorf("%s: %s %s", in, u, s)
		}
	}
}
