package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

// The operator's pins (ADR 0015) through the commands: `provider pin` shows what to pin and whether
// the file does; a plan runs a provider only as pinned, says so when it is not pinned at all, and
// strict_pins refuses it.
func TestProviderPins(t *testing.T) {
	tools := toolchainDir(t)
	var requests atomic.Int32
	api := kvapi.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	program, err := os.ReadFile("../../examples/kv/pome/provider.wasm")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(program)
	good := hex.EncodeToString(sum[:])
	catalog, _ := filepath.Abs("../../examples/kv-demo/kv-catalog.json")
	dir := t.TempDir()
	config := func(top, pin string) string {
		p := filepath.Join(dir, "mamela.providers.toml")
		os.WriteFile(p, []byte(top+"[provider.kv-example]\napi_url = \""+srv.URL+"\"\ncatalog = \""+catalog+"\"\n"+pin), 0o600)
		return p
	}
	mamela := func(args ...string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	pome := "../../examples/kv/pome"

	// provider pin: the digest to put in the file, and what the file says of it.
	code, out := mamela("provider", "pin", "--providers", config("", ""), pome)
	if code != 0 || !strings.Contains(out, "[provider.kv-example]\nprogram_sha256 = \""+good+"\"") || !strings.Contains(out, "not pinned") ||
		!strings.Contains(out, "strict_pins") {
		t.Fatalf("provider pin: %d\n%s", code, out)
	}
	if code, out := mamela("provider", "pin", "--check", "--providers", config("", ""), pome); code != 1 {
		t.Fatalf("--check with no pin: %d\n%s", code, out)
	}
	if code, out := mamela("provider", "pin", "--check", "--providers", config("", "program_sha256 = \""+good+"\"\n"), pome); code != 0 || !strings.Contains(out, ": pinned") {
		t.Fatalf("--check with the pin: %d\n%s", code, out)
	}
	wrong := strings.Repeat("0", 64)
	if code, out := mamela("provider", "pin", "--check", "--providers", config("", "program_sha256 = \""+wrong+"\"\n"), pome); code != 1 || !strings.Contains(out, ": differs") {
		t.Fatalf("--check with another pin: %d\n%s", code, out)
	}
	if code, out := mamela("provider", "pin", "--providers", config("", ""), pome, "nobody"); code != 1 || !strings.Contains(out, "no provider nobody") {
		t.Fatalf("an unknown provider: %d\n%s", code, out)
	}

	plan := func(providers string) (int, string) {
		t.Helper()
		return mamela("plan", "--db", filepath.Join(t.TempDir(), "state.db"), "--providers", providers, "--toolchain", tools, "../../examples/kv-demo")
	}
	// Not pinned: as before, with the warning.
	if code, out := plan(config("", "")); code != 0 || !strings.Contains(out, "MAM-PRV-0010") || !strings.Contains(out, "mamela provider pin") {
		t.Fatalf("plan, not pinned: %d\n%s", code, out)
	}
	// Pinned to another program: the declaration's Pome is not the one the operator chose.
	before := requests.Load()
	if code, out := plan(config("", "program_sha256 = \""+wrong+"\"\n")); code != 1 || !strings.Contains(out, "MAM-PRV-0009") || requests.Load() != before {
		t.Fatalf("plan, pinned to another program: %d, %d requests\n%s", code, requests.Load()-before, out)
	}
	// Pinned to this one.
	if code, out := plan(config("", "program_sha256 = \""+good+"\"\n")); code != 0 || strings.Contains(out, "MAM-PRV-0010") {
		t.Fatalf("plan, pinned: %d\n%s", code, out)
	}
	// strict_pins, with and without the pin.
	if code, out := plan(config("strict_pins = true\n", "")); code != 1 || !strings.Contains(out, "MAM-PRV-0010") {
		t.Fatalf("plan, strict_pins without a pin: %d\n%s", code, out)
	}
	if code, out := plan(config("strict_pins = true\n", "program_sha256 = \""+good+"\"\n")); code != 0 {
		t.Fatalf("plan, strict_pins with the pin: %d\n%s", code, out)
	}
}
