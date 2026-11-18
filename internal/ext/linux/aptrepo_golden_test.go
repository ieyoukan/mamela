package linux

// The golden plans of NodeAptRepo, as those of NodeFile (golden_test.go): each scenario's
// declaration, observation and policy, and the plan made of them, under
// testdata/golden/plan/apt-<scenario>. Regenerate them only for a change that means to change them:
//
//	go test ./internal/ext/linux -run Golden -update

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/state"
)

const dockerCE = `{"kind":"NodePackage","name":"web/docker-ce","spec":{"node":"web","package":"docker-ce","state":"Present"}}`

// dockerState is what mamelet reports of the source "docker": its files as given, apt has read it.
func dockerState(sources, asc, gpg FileState) AptRepoState {
	sources.Path = AptSourcesPath("docker")
	asc.Path, gpg.Path = AptKeyPath("docker", true), AptKeyPath("docker", false)
	return AptRepoState{Name: "docker", Sources: sources, KeyArmored: asc, KeyBinary: gpg}
}

// dockerSources is the file of the source as declared (dockerRepo), its key in .asc or .gpg.
func dockerSources(armored bool) string {
	g, err := decodeGraph(dockerRepo)
	if err != nil {
		panic(err)
	}
	return AptSources(*AptRepoOf(g.Resources[1]), AptKeyPath("docker", armored))
}

func TestGoldenAptRepoPlans(t *testing.T) {
	reported := func(repos ...AptRepoState) *Observed { return &Observed{AptRepos: repos} }
	now := world{mamelet: Release + "+abc", accepts: Version}
	with := func(w world, f func(w *world)) world { f(&w); return w }
	unread := []string{"https://download.docker.com/linux/ubuntu noble"}
	missing := FileState{Missing: []string{"/etc/apt/keyrings"}}
	inSync := dockerState(present("", dockerSources(true)), present("", testKeyArmored), FileState{})
	locate := map[string]state.Package{"web/docker-ce": {Error: "simulate: /usr/bin/apt-get -s install docker-ce: exit 100: E: Unable to locate package docker-ce", ObservedAt: gt0}}
	cs := noderev.NewChangeSet([]noderev.Change{
		{Action: noderev.ChangeInstall, Package: "containerd.io", Version: "1.7.27-1", Origins: []string{"Docker CE:noble"}},
		{Action: noderev.ChangeInstall, Package: "docker-ce", Version: "5:28.3.3-1~ubuntu.24.04~noble", Origins: []string{"Docker CE:noble"}},
	})
	simulated := map[string]state.Package{"web/docker-ce": {ChangeSet: &cs, ObservedAt: gt0}}
	docker := policy.Default
	docker.AllowedAptOrigins = []string{"Docker CE:", "Ubuntu:"}

	for _, s := range []struct {
		name      string
		resources []string
		world     world
		packages  map[string]state.Package
		pol       policy.Policy
	}{
		{"apt-create", []string{dockerRepo}, with(now, func(w *world) {
			s := dockerState(FileState{}, missing, missing)
			s.Unread = unread
			w.observed = reported(s)
		}), nil, policy.Default},
		{"apt-in-sync", []string{dockerRepo}, with(now, func(w *world) { w.observed = reported(inSync) }), nil, policy.Default},
		{"apt-key-changed", []string{dockerRepo}, with(now, func(w *world) {
			w.observed = reported(dockerState(present("", dockerSources(true)), present("", "the key before\n"), FileState{}))
		}), nil, policy.Default},
		{"apt-sources-changed", []string{dockerRepo}, with(now, func(w *world) {
			w.observed = reported(dockerState(present("", "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: jammy\n"), present("", testKeyArmored), FileState{}))
		}), nil, policy.Default},
		{"apt-binary-key-other-form", []string{dockerRepo}, with(now, func(w *world) {
			// The declared key is in .gpg (the key served is binary); an older one is left in .asc.
			w.observed = reported(dockerState(present("", dockerSources(false)), present("", "the key before\n"), present("", testKeyArmored)))
		}), nil, policy.Default},
		{"apt-unread", []string{dockerRepo}, with(now, func(w *world) {
			s := inSync
			s.Unread = unread
			w.observed = reported(s)
		}), nil, policy.Default},
		{"apt-link-blocked", []string{dockerRepo}, with(now, func(w *world) {
			s := dockerState(FileState{Exists: true, Unsafe: "/etc/apt/sources.list.d/mamela-docker.sources is a symbolic link"}, missing, missing)
			w.observed = reported(s)
		}), nil, policy.Default},
		{"apt-conflict", []string{dockerRepo}, with(now, func(w *world) {
			s := dockerState(FileState{}, missing, missing)
			s.Conflicts = []string{"/etc/apt/sources.list.d/docker.list"}
			w.observed = reported(s)
		}), nil, policy.Default},
		{"apt-package-waits", []string{dockerRepo, dockerCE}, with(now, func(w *world) {
			s := dockerState(FileState{}, missing, missing)
			s.Unread = unread
			w.observed = reported(s)
		}), locate, policy.Default},
		{"apt-package-after-repo", []string{dockerRepo, dockerCE}, with(now, func(w *world) { w.observed = reported(inSync) }), simulated, docker},
		{"apt-package-origin-denied", []string{dockerRepo, dockerCE}, with(now, func(w *world) { w.observed = reported(inSync) }), simulated, policy.Default},
		{"apt-old-mamelet", []string{dockerRepo, dockerCE}, with(now, func(w *world) { w.mamelet, w.accepts = "0.9.2", 0 }), locate, policy.Default},
		{"apt-kept", []string{dockerCE}, with(now, func(w *world) { w.observed = reported(inSync) }), simulated, docker},
	} {
		t.Run(s.name, func(t *testing.T) {
			g, err := decodeGraph(s.resources...)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
			if s.packages != nil {
				for k, v := range s.packages {
					snap.Packages[k] = v
				}
				if snap, err = state.Decode(snap.Canonical()); err != nil {
					t.Fatal(err)
				}
			}
			providers := goldenProviders
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
			// A source is added only with an approval, and a package waits for a source that is not in place.
			for _, c := range p.Changes {
				switch c.Kind {
				case KindAptRepo:
					if c.Status == plan.StatusChange && (c.Verdict != policy.ApprovalRequired || !slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == OpAddAptRepo })) {
						t.Errorf("a source is added without an approval: %+v", c)
					}
				case graph.KindNodePackage:
					repo := slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Kind == KindAptRepo && c.Status != plan.StatusRetained })
					if repo >= 0 && p.Changes[repo].Status != plan.StatusInSync && (c.Status != plan.StatusWaiting || c.Note.Code != codeAptRepoPackageGate) {
						t.Errorf("a package does not wait for its node's source: %+v", c)
					}
				}
			}
		})
	}
}
