package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A native provider runs only after it is trusted, as that exact build, source and version.
func TestProviderTrustNative(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "kv-provider")
	if b, err := exec.Command("go", "build", "-o", program, "../../examples/kv/cmd/kv-provider").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	trustFile := filepath.Join(dir, "mamela.trust.toml")
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	if code, out := run("provider", "inspect", "--trust-file", trustFile, "--source", "example.com/kv", "--version", "0.1.0", program); code != 1 || !strings.Contains(out, "MAM-PRV-0002") {
		t.Fatalf("an untrusted native provider: %d\n%s", code, out)
	}
	if code, out := run("provider", "trust", "--trust-file", trustFile, "--source", "example.com/kv", "--version", "0.1.0", program); code != 0 || !strings.Contains(out, "outside any sandbox") {
		t.Fatalf("trust: %d\n%s", code, out)
	}
	if code, out := run("provider", "inspect", "--trust-file", trustFile, "--source", "example.com/kv", "--version", "0.1.0", program); code != 0 || !strings.Contains(out, "KVItem create-item") {
		t.Fatalf("a trusted native provider: %d\n%s", code, out)
	}
	if code, out := run("provider", "inspect", "--trust-file", trustFile, "--source", "example.com/kv", "--version", "0.2.0", program); code != 1 || !strings.Contains(out, "MAM-PRV-0002") {
		t.Fatalf("trusted as another version: %d\n%s", code, out)
	}
	if err := os.WriteFile(program, append([]byte("#!/bin/sh\n"), []byte("exit 0\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := run("provider", "inspect", "--trust-file", trustFile, "--source", "example.com/kv", "--version", "0.1.0", program); code != 1 || !strings.Contains(out, "MAM-PRV-0002") {
		t.Fatalf("another build under the trusted name: %d\n%s", code, out)
	}
}

// inspect classifies a provider's calls with the catalogs its Pome ships (ADR 0008), and with
// --catalog files; catalog shows them, for the operator to review before naming one.
func TestProviderInspectCatalogs(t *testing.T) {
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	pome := "../../examples/ec2/pome"
	code, out := run("provider", "inspect", pome)
	if code != 0 || strings.Contains(out, "unclassified") || !strings.Contains(out, "POST / Action=RunInstances MaxCount=1 MinCount=1") {
		t.Fatalf("the EC2 example's Pome: %d\n%s", code, out)
	}
	if code, out := run("provider", "inspect", "--catalog", "pome/catalogs/ec2.json", "../../examples/ec2/pome/provider.wasm"); code != 1 {
		t.Fatalf("a missing catalog file: %d\n%s", code, out)
	}
	if code, out := run("provider", "inspect", "--catalog", "../../examples/ec2/pome/catalogs/ec2.json", pome); code != 1 || !strings.Contains(out, "a second catalog") {
		t.Fatalf("two catalogs of one api: %d\n%s", code, out)
	}
	if code, out := run("provider", "inspect", "--catalog", "builtin:ec2", "../../examples/ec2/pome/provider.wasm"); code != 1 || !strings.Contains(out, "ADR 0008") {
		t.Fatalf("a builtin catalog: %d\n%s", code, out)
	}
	// The program alone classifies nothing.
	if code, out := run("provider", "inspect", "../../examples/ec2/pome/provider.wasm"); code != 0 || !strings.Contains(out, "unclassified") {
		t.Fatalf("the program alone: %d\n%s", code, out)
	}
	code, out = run("provider", "catalog", pome)
	if code != 0 || !strings.Contains(out, `catalog = "pome:ec2"  api ec2`) || !strings.Contains(out, "lose-data POST / Action=TerminateInstances  bind InstanceId.1") ||
		!strings.Contains(out, "measures cpu by InstanceType (table)") {
		t.Fatalf("catalog: %d\n%s", code, out)
	}
	if code, out := run("provider", "catalog", "--json", pome); code != 0 || !strings.HasPrefix(out, `{`) || !strings.Contains(out, `"schema": "mamela-catalog/v1"`) {
		t.Fatalf("catalog --json: %d\n%s", code, out)
	}
	// A declaration's providers.
	if code, out := run("provider", "catalog", "../../examples/homelab", "ec2-example"); code != 0 || !strings.Contains(out, "provider ec2-example 0.1.0") {
		t.Fatalf("catalog of a declaration: %d\n%s", code, out)
	}
}
