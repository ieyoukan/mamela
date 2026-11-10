package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

// A declaration that imports a provider's Pome is planned, approved and applied through the
// provider: the plan shows the exact requests, nothing runs without approval, the API gets
// exactly them, and a change of the world after planning stops the apply.
func TestPlanAndApplyThroughAProvider(t *testing.T) {
	tools := toolchainDir(t)
	api := kvapi.New()
	srv := httptest.NewServer(kvapi.RequireToken("s3cret", api))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "token"), []byte("s3cret\n"), 0o600)
	catalog, _ := filepath.Abs("../../examples/kv-demo/kv-catalog.json")
	config := filepath.Join(dir, "mamela.providers.toml")
	os.WriteFile(config, []byte(`[provider.kv-example]
api_url = "`+srv.URL+`"
token_file = "token"
catalog = "`+catalog+`"
`), 0o600)
	db := filepath.Join(dir, "state.db")
	mamela := func(args ...string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	planDigest := regexp.MustCompile(`mamela approve (sha256:[0-9a-f]+)`)
	plan := func() (string, string) {
		t.Helper()
		code, out := mamela("plan", "--db", db, "--providers", config, "--toolchain", tools, "../../examples/kv-demo")
		if code != 0 {
			t.Fatalf("plan: %d\n%s", code, out)
		}
		return planDigest.FindStringSubmatch(out)[1], out
	}

	digest, out := plan()
	for _, want := range []string{"+ KVItem web  [needs approval]  via kv-example", "MAM-POL-0209",
		"sends PUT /items/{item} owner=mamela value=v1", "sends PUT /items/{item}/labels labels=prod", "+ KVItem db"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "KVItem db") && strings.Contains(out, "labels=\n") {
		t.Errorf("an empty labels request:\n%s", out)
	}

	if code, out := mamela("apply", "--db", db, "--providers", config, "--toolchain", tools, digest); code != 0 || !strings.Contains(out, "needs-approval") {
		t.Fatalf("apply without approval: %d\n%s", code, out)
	}
	if n := len(api.Items()); n != 0 {
		t.Fatalf("%d items without approval", n)
	}
	mamela("approve", "--db", db, "--by", "tester", digest)
	if code, out := mamela("apply", "--db", db, "--providers", config, "--toolchain", tools, digest); code != 0 {
		t.Fatalf("apply: %d\n%s", code, out)
	}
	items := api.Items()
	if web := items["web"]; web.Value != "v1" || web.Owner != "mamela" || strings.Join(web.Labels, ",") != "prod" || items["db"].Value != "v1" {
		t.Fatalf("the API has %+v", items)
	}
	if _, out := plan(); !strings.Contains(out, "= KVItem web  [in sync]") {
		t.Errorf("after apply, the plan is not in sync:\n%s", out)
	}

	// The world changes after planning: the approved change is based on another version.
	api.Put(kvapi.Item{Name: "web", Value: "drifted", Owner: "mamela", Labels: []string{"prod"}, Version: 7})
	digest, out = plan()
	if !strings.Contains(out, "based on version 7") {
		t.Fatalf("the update is not based on the observed version:\n%s", out)
	}
	mamela("approve", "--db", db, "--by", "tester", digest)
	api.Put(kvapi.Item{Name: "web", Value: "someone else's", Owner: "mamela", Labels: []string{"prod"}, Version: 8})
	code, out := mamela("apply", "--db", db, "--providers", config, "--toolchain", tools, digest)
	if code == 0 || !strings.Contains(out, "MAM-REC-0002") {
		t.Fatalf("apply after the world changed: %d\n%s", code, out)
	}
	if v := api.Items()["web"].Value; v != "someone else's" {
		t.Errorf("the newer value was overwritten: %q", v)
	}
}
