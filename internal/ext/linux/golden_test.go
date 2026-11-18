package linux

// The golden plans of NodeFile: each scenario is a declaration, an observation and a policy (written
// to testdata/golden/plan/<scenario> as they are), and the plan made of them: its canonical JSON
// (what an approval's digest covers), its digest, how it is shown, and what applying it would use.
// The core's golden plans (internal/plan/testdata/golden) are made without this extension and do not
// change with it. Regenerate them only for a change that means to change what they fix:
//
//	go test ./internal/ext/linux -run Golden -update

import (
	"bytes"
	"encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const goldenDir = "testdata/golden"

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with -update to write it)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file:\n got: %s\nwant: %s", name, got, want)
	}
}

// manifestWith is the node provider's manifest with, of the linux kinds, only those named: the golden
// plans of a kind are made with the contract of the core and that kind (its digest is in every plan),
// so that a kind added later does not change them, as the core's are made without the extension.
func manifestWith(keep ...graph.Kind) spec.Manifest {
	m := node.Manifest()
	m.Kinds = slices.DeleteFunc(slices.Clone(m.Kinds), func(k spec.Kind) bool {
		_, linux := kindOf(k.Kind)
		return linux && !slices.Contains(keep, k.Kind)
	})
	return m
}

var gt0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const (
	appConf = "listen 8080\nworkers 4\n"
	oldConf = "listen 80\n"
)

// world is what a scenario observes: the machine web (Mamela's, running), its mamelet, and what that
// reported of linux.
type world struct {
	mamelet  string // the mamelet release web runs ("" when it has not reported)
	accepts  int    // the linux version it takes (0: it lists none)
	refused  string
	observed *Observed
	at       time.Time // when the report arrived
	young    bool      // the machine has run for a minute (not an hour)
	services map[string][2]bool
}

func (w world) snapshot(g *graph.Graph) state.Snapshot {
	s := state.Empty(gt0.Add(time.Minute))
	provided := map[string]state.Provided{}
	uptime := int64(3600)
	if w.young {
		uptime = 60
	}
	for _, r := range g.Resources {
		if r.Provided != nil {
			provided[r.ID()] = state.Provided{Provider: r.Provided.Provider, Kind: r.Kind, Name: r.Name, Declared: true,
				SpecDigest: state.SpecDigest(r.Provided.Spec), ObservedAt: gt0,
				Observed: protocol.Observed{ID: r.ID(), Kind: r.Kind, Name: r.Name, Exists: true, Owned: true, IDs: map[string]string{"vmid": "7001"},
					Node: &protocol.ObservedNode{ID: 7001, Running: true, UptimeSeconds: uptime}}}
		}
	}
	s.AddProvided(provided)
	at := w.at
	if at.IsZero() {
		at = gt0
	}
	if w.mamelet != "" {
		s.Mamelets = map[string]state.Mamelet{"web/mamelet": {SHA256: strings.Repeat("ab", 32), Version: w.mamelet, ObservedAt: at}}
	}
	for name, sv := range w.services {
		s.Services["web/"+name] = state.Service{Exists: true, Enabled: true, Running: sv[1], RepairStart: false, ObservedAt: at}
	}
	if w.accepts > 0 {
		o := state.ExtObservation{Accepts: w.accepts, Refused: w.refused, ObservedAt: at}
		if w.observed != nil {
			o.Payload = encode(*w.observed)
		}
		s.Ext = map[string]map[string]state.ExtObservation{"web": {Name: o}}
	}
	n, err := state.Decode(s.Canonical()) // as stored and read again
	if err != nil {
		panic(err)
	}
	return n
}

func accountsOf(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		exists := !strings.HasPrefix(n, "!")
		m[strings.TrimPrefix(n, "!")] = exists
	}
	return m
}

// present is a regular file with this content, 0644, root's.
func present(path, content string) FileState {
	return FileState{Path: path, Exists: true, SHA256: ContentSHA256(content), Mode: "0644", Owner: "root", Group: "root"}
}

func TestGoldenFilePlans(t *testing.T) {
	appFile := fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","content":"listen 8080\nworkers 4\n","restart":["app"]}`)
	appService := `{"kind":"NodeService","name":"web/app","spec":{"node":"web","service":"app","enabled":true,"running":true}}`
	root := accountsOf("root")
	reported := func(files ...FileState) *Observed {
		return &Observed{Files: files, Users: root, Groups: root}
	}
	now := world{mamelet: Release + "+abc", accepts: Version, services: map[string][2]bool{"app": {true, true}}}
	with := func(w world, f func(w *world)) world { f(&w); return w }
	pol := policy.Default
	protected := policy.Default
	protected.ProtectedNodes = []string{"web"}

	for _, s := range []struct {
		name      string
		resources []string
		world     world
		pol       policy.Policy
	}{
		{"create", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/app/app.conf", Missing: []string{"/etc/app"}})
		}), pol},
		{"change-content", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", oldConf))
		}), pol},
		{"change-mode-only", []string{appFile, appService}, with(now, func(w *world) {
			f := present("/etc/app/app.conf", appConf)
			f.Mode, f.Group = "0600", "adm"
			w.observed = reported(f)
		}), pol},
		{"in-sync", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", appConf))
		}), pol},
		{"symlink", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/app/app.conf", Exists: true, Unsafe: "/etc/app/app.conf is a symbolic link"})
		}), pol},
		{"large", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/app/app.conf", Exists: true, Large: true, Mode: "0644", Owner: "root", Group: "root"})
		}), pol},
		{"missing-owner", []string{fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"app","group":"app"}`)}, with(now, func(w *world) {
			w.observed = &Observed{Files: []FileState{{Path: "/etc/app.conf"}}, Users: accountsOf("root", "!app"), Groups: accountsOf("root", "!app")}
		}), pol},
		{"owner-not-observed-yet", []string{fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"app"}`)}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/app.conf"})
		}), pol},
		{"old-mamelet", []string{appFile, appService}, with(now, func(w *world) {
			w.mamelet, w.accepts = "0.9.2", 0
		}), pol},
		{"refused-part", []string{appFile, appService}, with(now, func(w *world) {
			w.refused = "invalid linux part: file \"/etc/app/app.conf\": the content is not the one approved (sha256 0000)"
		}), pol},
		{"stale", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", oldConf))
			w.at = gt0.Add(-time.Hour)
		}), pol},
		{"awaiting-report", []string{appFile, appService}, with(now, func(w *world) { w.observed = reported() }), pol},
		{"agent-not-reported", []string{appFile}, world{young: true}, pol},
		{"agent-silent", []string{appFile}, world{}, pol},
		{"kept", []string{appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", appConf), FileState{Path: "/etc/gone.conf"})
		}), pol},
		{"start-waits-for-file", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", oldConf))
			w.services = map[string][2]bool{"app": {true, false}}
		}), pol},
		{"unit", []string{fileRes("web/file/etc/systemd/system/app.service", `{"node":"web","path":"/etc/systemd/system/app.service","content":"[Service]\nExecStart=/usr/local/bin/app\n","restart":["app"]}`), appService}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/systemd/system/app.service"})
		}), pol},
		{"protected-node", []string{appFile, appService}, with(now, func(w *world) {
			w.observed = reported(present("/etc/app/app.conf", oldConf))
		}), protected},
		{"secret-warning", []string{fileRes("web/file/etc/app/key.pem", `{"node":"web","path":"/etc/app/key.pem","content":"-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n","mode":"0600"}`)}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/app/key.pem"})
		}), pol},
	} {
		t.Run(s.name, func(t *testing.T) {
			g, err := decodeGraph(s.resources...)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
			providers := spec.Providers{manifestWith(KindFile)}.Digest()
			p := plan.BuildFor(g, snap, s.pol, providers)
			dir := "plan/" + s.name + "/"
			golden(t, dir+"graph.json", append(g.Canonical(), '\n'))
			golden(t, dir+"snapshot.json", append(snap.Canonical(), '\n'))
			golden(t, dir+"policy.json", append(s.pol.Canonical(), '\n'))
			golden(t, dir+"plan.json", append(p.Canonical(), '\n'))
			golden(t, dir+"digest.txt", []byte("plan "+p.Digest+"\ngraph "+g.Digest()+"\nsnapshot "+snap.Digest()+"\n"))
			var r bytes.Buffer
			plan.Render(&r, p)
			golden(t, dir+"render.txt", r.Bytes())
			req, err := json.Marshal((&site.Site{}).PlannedRequirements(p, g, s.pol), json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			golden(t, dir+"requirements.json", append(req, '\n'))
			if q := plan.BuildFor(g, snap, s.pol, providers); q.Digest != p.Digest {
				t.Error("the plan is not deterministic")
			}
			// What the scenario is about, besides the bytes.
			c := p.Changes[slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Kind == KindFile || s.name == "kept" })]
			if c.Status == plan.StatusChange && (c.Verdict == policy.Auto || !slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == OpWriteFile })) {
				t.Errorf("a file is written without an approval: %+v", c)
			}
		})
	}
}
