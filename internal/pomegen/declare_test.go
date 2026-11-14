package pomegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

func imported(addr string, kind protocol.ResourceKind, name, spec string) protocol.Imported {
	return protocol.Imported{Address: addr, Kind: kind, Name: name, Spec: []byte(spec), IDs: map[string]string{"id": name}}
}

func cloudImports() []Imports {
	return []Imports{{Manifest: rich(), Module: "Cloud", Resources: []protocol.Imported{
		// Listed before what it refers to: the output puts what is referred to first.
		imported("cloud_vm.app", "CloudVM", "app", `{"network":"main","groups":["web"],"size":"large-2x","cpus":2,"public":true,"tags":["a \"b\" #{c}"]}`),
		imported("cloud_sg.web", "CloudSecurityGroup", "web", `{"network":"main","rules":[{"port":443,"protocol":"tcp","fromNetwork":"main"},{"port":53,"protocol":"udp"}]}`),
		imported("cloud_network.main", "CloudNetwork", "main", `{"cidr":"10.0.0.0/16"}`),
	}}}
}

func TestDeclarations(t *testing.T) {
	src, err := Declarations(cloudImports(), DeclareOptions{Source: "a Terraform state"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"import Cloud.{CloudNetwork, CloudSecurityGroup, CloudVm, Large2x, Rule, Tcp, Udp}",
		`  cloud_network_main = CloudNetwork(name: "main", cidr: "10.0.0.0/16")`,
		`rules: [Rule(port: 443, protocol: Tcp, from_network: Some(cloud_network_main)), Rule(port: 53, protocol: Udp, from_network: None)]`,
		`groups: [cloud_security_group_web], size: Large2x, cpus: Some(2), public: True, tags: ["a \"b\" \#{c}"], type_: None)`,
		"    |> Cloud.cloud_network(cloud_network_main)\n    |> Cloud.cloud_security_group(cloud_security_group_web)\n    |> Cloud.cloud_vm(cloud_vm_app)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("no %q in:\n%s", want, src)
		}
	}
	checkDeclaration(t, src)
}

func TestDeclarationsRefused(t *testing.T) {
	for name, c := range map[string]struct{ spec, want string }{
		"missing ref":   {`{"cidr":"x","network":"other"}`, ""},
		"unknown field": {`{"cidr":"x","extra":1}`, "does not declare"},
		"missing field": {`{}`, "requires"},
		"wrong type":    {`{"cidr":1}`, "not a string"},
	} {
		im := []Imports{{Manifest: rich(), Module: "Cloud", Resources: []protocol.Imported{imported("x.y", "CloudNetwork", "n", c.spec)}}}
		if _, err := Declarations(im, DeclareOptions{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	vm := []Imports{{Manifest: rich(), Module: "Cloud", Resources: []protocol.Imported{
		imported("cloud_vm.app", "CloudVM", "app", `{"network":"gone","groups":[],"size":"small","public":false}`)}}}
	if _, err := Declarations(vm, DeclareOptions{}); err == nil || !strings.Contains(err.Error(), "not imported") {
		t.Errorf("a reference to what is not imported: %v", err)
	}
	vm[0].Resources[0].Spec = []byte(`{"network":"main","groups":[],"size":"huge","public":false}`)
	vm[0].Resources = append(vm[0].Resources, imported("cloud_network.main", "CloudNetwork", "main", `{"cidr":"x"}`))
	if _, err := Declarations(vm, DeclareOptions{}); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("a value not in the enum: %v", err)
	}
}

// checkDeclaration compiles the imported declarations against the generated Pome of the provider:
// a provider Pome (lib.emel generated) and a declaration that depends on it.
func checkDeclaration(t *testing.T, imported []byte) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	emela := filepath.Join(root, ".tools/bin/emela")
	if _, err := os.Stat(emela); err != nil {
		t.Skip("no toolchain: run scripts/install-toolchain.sh")
	}
	lib, err := Generate(rich(), Options{Module: "Cloud"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lock, _ := os.ReadFile(filepath.Join(root, "examples/kv/pome/Pome.lock"))
	files := map[string]string{
		"cloud/Pome.toml": `[pome]
name = "example.com/cloud"
version = "0.1.0"
emela = "0.27.0"
module = "Cloud"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.3.0"
"github.com/emela-lang/x/dynamic" = "v0.5.1"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "` + filepath.Join(root, "pome/mamela") + `" }
`,
		"cloud/Pome.lock":    string(lock),
		"cloud/src/lib.emel": string(lib),
		"decl/Pome.toml": `[pome]
name = "example.com/decl"
version = "0.1.0"
emela = "0.27.0"

[build]
target = "wasm"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.3.0"
"example.com/cloud" = "v0.1.0"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "` + filepath.Join(root, "pome/mamela") + `" }
"example.com/cloud" = { path = "../cloud" }
`,
		"decl/Pome.lock":         string(lock),
		"decl/src/imported.emel": string(imported),
		"decl/src/main.emel": `import Std.Console.{SystemConsole}
import Mamela
import Imported

fn main() {
  with SystemConsole {
    Mamela.empty() |> Imported.imported |> Mamela.print
  }
}
`,
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(emela, "check", filepath.Join(dir, "decl"))
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "EMELA_POMEPROXY=off", "EMELA_POMESUMDB=off",
		"EMELA_POME_CACHE=" + filepath.Join(root, ".tools/pomes")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the imported declarations do not compile: %v\n%s\n%s", err, out, numbered(imported))
	}
}
