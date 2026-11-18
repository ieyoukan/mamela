package linux

import (
	"testing"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

// findChange is the change of resource id in p.
func findChange(t *testing.T, p *plan.Plan, id string) plan.Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no change of %s in %+v", id, p.Changes)
	return plan.Change{}
}

// A file and an artifact owned by a user that a package of the declaration makes (the package
// prometheus makes the user prometheus) wait for the package, as a NodeUser does for its groups; once
// the package is installed and the user is still not there, they are blocked. Blocked would stop
// mamelacd's sync = "push" from applying the package that resolves it.
func TestOwnersAPackageMakesAreWaitedFor(t *testing.T) {
	pkg := `{"kind":"NodePackage","name":"web/prometheus","spec":{"node":"web","package":"prometheus","state":"Present"}}`
	file := fileRes("web/file/etc/prometheus/prometheus.yml", `{"node":"web","path":"/etc/prometheus/prometheus.yml","content":"global: {}\n","owner":"prometheus","group":"prometheus"}`)
	art := artifactRes("web/artifact/usr/local/bin/exporter", `{"node":"web","url":"https://example.com/exporter","sha256":"`+sumA+`","path":"/usr/local/bin/exporter","owner":"prometheus","group":"prometheus"}`)
	g, err := decodeGraph(pkg, file, art)
	if err != nil {
		t.Fatal(err)
	}
	o := &Observed{
		Files:     []FileState{{Path: "/etc/prometheus/prometheus.yml", Missing: []string{"/etc/prometheus"}}},
		Artifacts: []ArtifactState{{FileState: FileState{Path: "/usr/local/bin/exporter"}}},
		Users:     map[string]bool{"prometheus": false}, Groups: map[string]bool{"prometheus": false},
	}
	s := world{mamelet: Release, accepts: Version, observed: o}.snapshot(g)
	s.Packages = map[string]state.Package{"web/prometheus": {ObservedAt: gt0}}
	p := plan.Build(g, s, policy.Default)
	for id, code := range map[string]string{"NodeFile web/file/etc/prometheus/prometheus.yml": codeFileAccountComing, "NodeArtifact web/artifact/usr/local/bin/exporter": codeArtifactAccountComing} {
		if c := findChange(t, p, id); c.Status != plan.StatusWaiting || c.Note.Code != code {
			t.Errorf("%s while the package is not installed: %+v %+v", id, c, c.Note)
		}
	}
	s.Packages = map[string]state.Package{"web/prometheus": {Installed: true, ObservedAt: gt0}}
	p = plan.Build(g, s, policy.Default)
	for id, code := range map[string]string{"NodeFile web/file/etc/prometheus/prometheus.yml": codeFileNoAccount, "NodeArtifact web/artifact/usr/local/bin/exporter": codeArtifactNoAccount} {
		if c := findChange(t, p, id); c.Status != plan.StatusBlocked || c.Note.Code != code {
			t.Errorf("%s once the package is installed: %+v %+v", id, c, c.Note)
		}
	}
}

// A machine that has run for an hour, whose declaration has only linux resources so far (added after
// its mamelet last looked): its mamelet reports (which release it runs, and that it takes linux), so
// it is not silent, and the file waits for its next report instead of "never reported" (MAM-PLAN-0110).
func TestAReportingNodeIsNotSilent(t *testing.T) {
	g, err := decodeGraph(flowFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]world{
		"it takes linux, and observed nothing yet": {mamelet: Release, accepts: Version},
		"it observed nothing of the file":          {mamelet: Release, accepts: Version, observed: &Observed{}},
		"it said only what it observed of linux":   {accepts: Version, observed: &Observed{}},
	} {
		p := plan.Build(g, w.snapshot(g), policy.Default)
		if c := findChange(t, p, "NodeFile web/file/etc/app/app.conf"); c.Status != plan.StatusWaiting || c.Note.Code == "MAM-PLAN-0110" {
			t.Errorf("%s: %+v %+v", name, c, c.Note)
		}
	}
	// One that never reported anything is silent.
	p := plan.Build(g, world{}.snapshot(g), policy.Default)
	if c := findChange(t, p, "NodeFile web/file/etc/app/app.conf"); c.Status != plan.StatusBlocked || c.Note.Code != "MAM-PLAN-0110" {
		t.Errorf("silent: %+v %+v", c, c.Note)
	}
}
