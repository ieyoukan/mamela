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
