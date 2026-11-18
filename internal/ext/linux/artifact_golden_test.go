package linux

// The golden plans of NodeArtifact, made as NodeFile's are (golden_test.go), in
// testdata/golden/plan/artifact-<scenario>. Regenerate them only for a change that means to change
// what they fix:
//
//	go test ./internal/ext/linux -run Golden -update

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/site"
)

// The sha256s of the golden scenarios: placeholders (not the releases' real ones), where a
// declaration would copy them from a release's checksum file.
var (
	goldenSum    = strings.Repeat("12", 32) // of the download declared
	goldenOldSum = strings.Repeat("34", 32) // of the download installed before
	goldenMember = strings.Repeat("56", 32) // of the member installed
)

func TestGoldenArtifactPlans(t *testing.T) {
	const path = "/usr/local/bin/node_exporter"
	exporter := artifactRes("web/artifact"+path, `{"node":"web","path":"`+path+`","url":"`+exporterURL+`","sha256":"`+goldenSum+`","extract":"`+exporterMember+`","restart":["node_exporter"]}`)
	service := `{"kind":"NodeService","name":"web/node_exporter","spec":{"node":"web","service":"node_exporter","enabled":true,"running":true}}`
	root := accountsOf("root")
	reported := func(as ...ArtifactState) *Observed { return &Observed{Artifacts: as, Users: root, Groups: root} }
	// installedFrom is the exporter at its path, as mamelet installed it from that download.
	installedFrom := func(url, sum, member string) ArtifactState {
		return ArtifactState{FileState: FileState{Path: path, Exists: true, SHA256: goldenMember, Mode: "0755", Owner: "root", Group: "root"},
			From: &Source{URL: url, SHA256: sum, Extract: member}}
	}
	now := world{mamelet: Release + "+abc", accepts: Version, services: map[string][2]bool{"node_exporter": {true, true}}}
	with := func(w world, f func(w *world)) world { f(&w); return w }
	userComing := func(_ []graph.Resource, node, name string, _ bool) (string, bool) {
		return "NodeUser " + node + "/user/" + name, name == "node-exp"
	}

	for _, s := range []struct {
		name      string
		resources []string
		world     world
		makers    []func(g []graph.Resource, node, name string, group bool) (string, bool)
	}{
		{"create", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(ArtifactState{FileState: FileState{Path: path}})
		}), nil},
		{"new-version", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(installedFrom(exporterOldURL, goldenOldSum, exporterOldMemb))
		}), nil},
		{"changed-by-hand", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(ArtifactState{FileState: FileState{Path: path, Exists: true, SHA256: sumB, Mode: "0755", Owner: "root", Group: "root"}})
		}), nil},
		{"mode-only", []string{exporter, service}, with(now, func(w *world) {
			a := installedFrom(exporterURL, goldenSum, exporterMember)
			a.Mode = "0700"
			w.observed = reported(a)
		}), nil},
		{"in-sync", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(installedFrom(exporterURL, goldenSum, exporterMember))
		}), nil},
		{"in-sync-from-a-mirror", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(installedFrom("https://mirror.example.com/node_exporter-1.8.2.linux-amd64.tar.gz", goldenSum, exporterMember))
		}), nil},
		{"symlink", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(ArtifactState{FileState: FileState{Path: path, Exists: true, Unsafe: path + " is a symbolic link"}})
		}), nil},
		{"missing-owner", []string{artifactRes("web/artifact"+path, `{"node":"web","path":"`+path+`","url":"`+exporterURL+`","sha256":"`+goldenSum+`","extract":"`+exporterMember+`","owner":"node-exp"}`)}, with(now, func(w *world) {
			w.observed = &Observed{Artifacts: []ArtifactState{{FileState: FileState{Path: path}}}, Users: accountsOf("root", "!node-exp"), Groups: root}
		}), nil},
		{"owner-coming", []string{artifactRes("web/artifact"+path, `{"node":"web","path":"`+path+`","url":"`+exporterURL+`","sha256":"`+goldenSum+`","extract":"`+exporterMember+`","owner":"node-exp"}`)}, with(now, func(w *world) {
			w.observed = &Observed{Artifacts: []ArtifactState{{FileState: FileState{Path: path}}}, Users: accountsOf("root", "!node-exp"), Groups: root}
		}), []func(g []graph.Resource, node, name string, group bool) (string, bool){userComing}},
		{"old-mamelet", []string{exporter, service}, with(now, func(w *world) {
			w.mamelet, w.accepts = "0.9.2", 0
		}), nil},
		{"kept", []string{service}, with(now, func(w *world) {
			w.observed = reported(installedFrom(exporterURL, goldenSum, exporterMember))
		}), nil},
		{"start-waits-for-artifact", []string{exporter, service}, with(now, func(w *world) {
			w.observed = reported(ArtifactState{FileState: FileState{Path: path}})
			w.services = map[string][2]bool{"node_exporter": {true, false}}
		}), nil},
	} {
		t.Run(s.name, func(t *testing.T) {
			if s.makers != nil {
				saved := accountMakers
				accountMakers = append(slices.Clone(accountMakers), s.makers...)
				t.Cleanup(func() { accountMakers = saved })
			}
			g, err := decodeGraph(s.resources...)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
			pol := policy.Default
			p := plan.BuildFor(g, snap, pol, goldenProviders)
			dir := "plan/artifact-" + s.name + "/"
			golden(t, dir+"graph.json", append(g.Canonical(), '\n'))
			golden(t, dir+"snapshot.json", append(snap.Canonical(), '\n'))
			golden(t, dir+"plan.json", append(p.Canonical(), '\n'))
			golden(t, dir+"digest.txt", []byte("plan "+p.Digest+"\ngraph "+g.Digest()+"\nsnapshot "+snap.Digest()+"\n"))
			var r bytes.Buffer
			plan.Render(&r, p)
			golden(t, dir+"render.txt", r.Bytes())
			req, err := json.Marshal((&site.Site{}).PlannedRequirements(p, g, pol), json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			golden(t, dir+"requirements.json", append(req, '\n'))
			if q := plan.BuildFor(g, snap, pol, goldenProviders); q.Digest != p.Digest {
				t.Error("the plan is not deterministic")
			}
			// What the scenario is about, besides the bytes.
			i := slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Kind == KindArtifact })
			if i < 0 {
				t.Fatalf("no artifact in %+v", p.Changes)
			}
			c := p.Changes[i]
			if c.Status == plan.StatusChange && (c.Verdict == policy.Auto || !slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == OpInstallArtifact })) {
				t.Errorf("an artifact is installed without an approval: %+v", c)
			}
		})
	}
}
