package mamelet

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

const nginxSim = `NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
Reading package lists...
The following NEW packages will be installed:
  nginx nginx-common
0 upgraded, 2 newly installed, 0 to remove and 3 not upgraded.
Inst nginx-common (1.24.0-2ubuntu7.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [all])
Inst nginx (1.24.0-2ubuntu7.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Conf nginx-common (1.24.0-2ubuntu7.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [all])
Conf nginx (1.24.0-2ubuntu7.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
`

type fakeService struct{ exists, enabled, running bool }

// fakeOS is a tiny Ubuntu: dpkg-query, apt-get and systemctl over in-memory state.
type fakeOS struct {
	installed   map[string]string
	sim         map[string]string
	services    map[string]*fakeService
	provides    map[string]string // installing a package creates this service
	failInstall bool
	commands    []string
}

func newFakeOS() *fakeOS {
	return &fakeOS{installed: map[string]string{}, sim: map[string]string{"nginx": nginxSim},
		services: map[string]*fakeService{}, provides: map[string]string{"nginx": "nginx.service"}}
}

func (f *fakeOS) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	f.commands = append(f.commands, program+" "+strings.Join(args, " "))
	last := args[len(args)-1]
	switch program {
	case progDpkgQuery:
		if v, ok := f.installed[last]; ok {
			return []byte("installed\t" + v + "\n"), nil, 0, nil
		}
		return nil, []byte("dpkg-query: no packages found matching " + last), 1, nil
	case progAptGet:
		if slices.Contains(args, "-s") {
			return []byte(f.sim[last]), nil, 0, nil
		}
		if f.failInstall {
			return nil, []byte("E: Could not get lock"), 100, nil
		}
		f.installed[last] = "1.24.0-2ubuntu7.4"
		if svc := f.provides[last]; svc != "" {
			f.services[svc] = &fakeService{exists: true, enabled: true, running: true}
		}
		return nil, nil, 0, nil
	case progSystemctl:
		s := f.services[last]
		if s == nil {
			s = &fakeService{}
		}
		switch args[0] {
		case "show":
			load, unit, active := "not-found", "", "inactive"
			if s.exists {
				load, unit = "loaded", "disabled"
				if s.enabled {
					unit = "enabled"
				}
				if s.running {
					active = "active"
				}
			}
			return []byte(fmt.Sprintf("LoadState=%s\nUnitFileState=%s\nActiveState=%s\n", load, unit, active)), nil, 0, nil
		case "enable":
			s.enabled = true
		case "start":
			s.running = true
		}
		return nil, nil, 0, nil
	}
	return nil, nil, -1, fmt.Errorf("unexpected program %s", program)
}

func (f *fakeOS) changes() []string {
	var out []string
	for _, c := range f.commands {
		if (strings.HasPrefix(c, progAptGet) && !strings.Contains(c, " -s ")) ||
			strings.HasPrefix(c, progSystemctl+" enable") || strings.HasPrefix(c, progSystemctl+" start") {
			out = append(out, c)
		}
	}
	return out
}

type fakeCP struct {
	signed  noderev.Signed
	err     error
	reports []noderev.Report
}

func (c *fakeCP) Revision(context.Context) (noderev.Signed, error) { return c.signed, c.err }
func (c *fakeCP) Report(_ context.Context, r noderev.Report) error {
	if c.err != nil {
		return c.err
	}
	c.reports = append(c.reports, r)
	return nil
}

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	key ed25519.PrivateKey
	os  *fakeOS
	cp  *fakeCP
	now time.Time
	a   *Agent
}

func newEnv(t *testing.T) *env {
	seed := make([]byte, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	e := &env{t: t, key: key, os: newFakeOS(), cp: &fakeCP{}, now: t0}
	e.a = &Agent{
		Config: Config{Node: "web", VMID: 7001, RevisionKey: key.Public().(ed25519.PublicKey), AllowedOrigins: []string{"Ubuntu:"}},
		CP:     e.cp, System: &System{Runner: e.os, AllowedOrigins: []string{"Ubuntu:"}},
		State: FileState{Path: filepath.Join(t.TempDir(), "state.json")}, Now: func() time.Time { return e.now }, Version: "test",
	}
	return e
}

func (e *env) publish(serial int64, pkgs []noderev.Package, svcs []noderev.Service) {
	e.t.Helper()
	s, err := noderev.Sign(e.key, noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: serial,
		IssuedAt: e.now, ExpiresAt: e.now.Add(time.Hour), Packages: pkgs, Services: svcs})
	if err != nil {
		e.t.Fatal(err)
	}
	e.cp.signed, e.cp.err = s, nil
}

func (e *env) round() noderev.Report {
	e.t.Helper()
	rep, err := e.a.Round(context.Background())
	if err != nil && e.cp.err == nil {
		e.t.Fatal(err)
	}
	return rep
}

func nginxChangeSet(t *testing.T) noderev.ChangeSet {
	changes, err := ParseSimulation(nginxSim)
	if err != nil {
		t.Fatal(err)
	}
	return noderev.NewChangeSet(changes)
}

const op1, op2, op3 = "op-000000000000000000000001", "op-000000000000000000000002", "op-000000000000000000000003"

func TestObservesAndReportsWhatInstallingWouldChange(t *testing.T) {
	e := newEnv(t)
	e.publish(1, []noderev.Package{{Name: "nginx"}}, []noderev.Service{{Name: "nginx.service", Enabled: true, Running: true}})
	rep := e.round()
	if len(e.os.changes()) != 0 {
		t.Fatalf("changed the system without approval: %v", e.os.changes())
	}
	p := rep.Packages[0]
	if p.Installed || p.ChangeSet == nil || p.ChangeSet.Digest != nginxChangeSet(t).Digest || len(p.ChangeSet.Changes) != 2 {
		t.Fatalf("package report %+v", p)
	}
	if rep.Services[0].Exists || rep.Serial != 1 || len(e.cp.reports) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestInstallsTheApprovedChangeSetOnce(t *testing.T) {
	e := newEnv(t)
	cs := nginxChangeSet(t)
	e.publish(2, []noderev.Package{{Name: "nginx", Install: &noderev.Install{Operation: op1, Attempt: 1, ChangeSet: cs.Digest}}},
		[]noderev.Service{{Name: "nginx.service", Enabled: true, Running: true}})
	rep := e.round()
	if !rep.Packages[0].Installed || len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeDone {
		t.Fatalf("report %+v", rep)
	}
	// The simulation ran right before the installation.
	var sawSim bool
	for _, c := range e.os.commands {
		if strings.Contains(c, " -s ") {
			sawSim = true
		}
		if strings.HasPrefix(c, progAptGet) && !strings.Contains(c, " -s ") && !sawSim {
			t.Fatal("installed without simulating first")
		}
	}
	// Removed by hand afterwards: the same attempt is not repeated; the result is still reported.
	delete(e.os.installed, "nginx")
	before := len(e.os.changes())
	rep = e.round()
	if len(e.os.changes()) != before || len(rep.Results) != 1 || rep.Results[0].Operation != op1 {
		t.Fatalf("repeated the attempt: %v, results %+v", e.os.changes(), rep.Results)
	}
}

func TestRefusesChangeSetsItMustNotInstall(t *testing.T) {
	approved := func(t *testing.T) string { return nginxChangeSet(t).Digest }
	for name, tc := range map[string]struct {
		sim    string
		digest func(*testing.T) string
		want   string
	}{
		"changed since approval": {strings.Replace(nginxSim, "7.4", "7.5", -1), approved, "plan again"},
		// Even a correctly approved change set is refused when it removes packages or comes from an
		// origin that is not allowed: the Control Plane could be wrong or compromised.
		"removes a package": {nginxSim + "Remv apache2 [2.4.58-1ubuntu8]\n", nil, "never removes"},
		"unknown origin": {strings.Replace(nginxSim, "Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64]",
			"ppa.example:noble [amd64]", 1), nil, "not an allowed APT origin"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.os.sim["nginx"] = tc.sim
			digest := ""
			if tc.digest != nil {
				digest = tc.digest(t)
			} else {
				changes, _ := ParseSimulation(tc.sim)
				digest = noderev.NewChangeSet(changes).Digest
			}
			e.publish(1, []noderev.Package{{Name: "nginx", Install: &noderev.Install{Operation: op1, Attempt: 1, ChangeSet: digest}}}, nil)
			rep := e.round()
			if len(e.os.changes()) != 0 {
				t.Fatalf("installed: %v", e.os.changes())
			}
			if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeRefused || !strings.Contains(rep.Results[0].Detail, tc.want) {
				t.Fatalf("results %+v", rep.Results)
			}
		})
	}
}

func TestFailedAttemptIsRetriedOnlyAsANewAttempt(t *testing.T) {
	e := newEnv(t)
	e.os.failInstall = true
	cs := nginxChangeSet(t)
	install := func(attempt int) []noderev.Package {
		return []noderev.Package{{Name: "nginx", Install: &noderev.Install{Operation: op1, Attempt: attempt, ChangeSet: cs.Digest}}}
	}
	e.publish(1, install(1), nil)
	if rep := e.round(); rep.Results[0].Outcome != noderev.OutcomeFailed || !strings.Contains(rep.Results[0].Detail, "lock") {
		t.Fatalf("results %+v", rep.Results)
	}
	e.round()
	if n := len(e.os.changes()); n != 1 {
		t.Fatalf("the failed attempt was repeated: %d installs", n)
	}
	e.os.failInstall = false
	e.publish(2, install(2), nil)
	if rep := e.round(); !rep.Packages[0].Installed || len(e.os.changes()) != 2 {
		t.Fatalf("attempt 2 did not install: %+v", rep)
	}
}

// Drift repair: an allowed service that stops is started again, also from the cached revision
// while the Control Plane is unreachable. One-time operations are never made offline.
func TestRepairsDriftAlsoOffline(t *testing.T) {
	e := newEnv(t)
	e.os.installed["nginx"] = "1.24.0-2ubuntu7.4"
	e.os.services["nginx.service"] = &fakeService{exists: true, enabled: false, running: false}
	e.publish(1, []noderev.Package{{Name: "nginx"}},
		[]noderev.Service{{Name: "nginx.service", Enabled: true, Running: true, Repair: []string{noderev.ActionStart}}})
	rep := e.round()
	if !e.os.services["nginx.service"].running || e.os.services["nginx.service"].enabled {
		t.Fatalf("service %+v (enable was not approved)", e.os.services["nginx.service"])
	}
	if !rep.Services[0].Running || rep.Results[0].Operation != "repair" {
		t.Fatalf("report %+v", rep)
	}

	e.cp.err = errors.New("connection refused")
	e.os.services["nginx.service"].running = false
	e.now = e.now.Add(30 * time.Minute)
	if _, err := e.a.Round(context.Background()); err == nil {
		t.Fatal("want the fetch error reported")
	}
	if !e.os.services["nginx.service"].running {
		t.Fatal("did not repair from the cached revision")
	}

	// A one-time enable in a revision that can no longer be fetched is not made.
	e.publish(2, []noderev.Package{{Name: "nginx"}}, []noderev.Service{{Name: "nginx.service", Enabled: true, Running: true,
		Once: []noderev.Once{{Operation: op2, Attempt: 1, Action: noderev.ActionEnable}}}})
	signed := e.cp.signed
	e.cp.err = errors.New("connection refused")
	st, _ := e.a.State.Load()
	st.Revision = &signed // as if cached, but this round is offline
	e.a.State.Save(st)
	e.a.Round(context.Background())
	if e.os.services["nginx.service"].enabled {
		t.Fatal("made a one-time change offline")
	}

	// After the cached revision expires, nothing is repaired any more.
	e.now = e.now.Add(2 * time.Hour)
	e.os.services["nginx.service"].running = false
	e.a.Round(context.Background())
	if e.os.services["nginx.service"].running {
		t.Fatal("repaired from an expired revision")
	}
}

func TestOneTimeServiceOperations(t *testing.T) {
	e := newEnv(t)
	e.os.installed["nginx"] = "1"
	e.os.services["nginx.service"] = &fakeService{exists: true}
	e.publish(1, nil, []noderev.Service{{Name: "nginx.service", Enabled: true, Running: true, Once: []noderev.Once{
		{Operation: op1, Attempt: 1, Action: noderev.ActionEnable}, {Operation: op2, Attempt: 1, Action: noderev.ActionStart}}}})
	rep := e.round()
	if s := e.os.services["nginx.service"]; !s.enabled || !s.running || len(rep.Results) != 2 {
		t.Fatalf("service %+v results %+v", s, rep.Results)
	}
	// Stopped by hand: the start was one-time, so it is not repeated (no standing repair).
	e.os.services["nginx.service"].running = false
	e.round()
	if e.os.services["nginx.service"].running {
		t.Fatal("repeated a one-time start")
	}
}

func TestRefusedRevisionsChangeNothing(t *testing.T) {
	e := newEnv(t)
	e.os.services["nginx.service"] = &fakeService{exists: true}
	start := []noderev.Service{{Name: "nginx.service", Running: true, Repair: []string{noderev.ActionStart}}}
	e.publish(5, nil, nil)
	e.round()

	other := ed25519.NewKeyFromSeed(append([]byte{1}, make([]byte, ed25519.SeedSize-1)...))
	forged, _ := noderev.Sign(other, noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 6,
		IssuedAt: e.now, ExpiresAt: e.now.Add(time.Hour), Services: start})
	e.cp.signed = forged
	rep := e.round()
	if !strings.Contains(rep.Refused, "signature") || len(e.os.changes()) != 0 {
		t.Fatalf("refused %q, changes %v", rep.Refused, e.os.changes())
	}

	e.publish(4, nil, start) // older than the accepted serial 5
	rep = e.round()
	if !strings.Contains(rep.Refused, "older") || len(e.os.changes()) != 0 {
		t.Fatalf("refused %q, changes %v", rep.Refused, e.os.changes())
	}
	if rep.Serial != 5 {
		t.Fatalf("kept working from serial %d, want the accepted 5", rep.Serial)
	}
}

func TestParseSimulation(t *testing.T) {
	changes, err := ParseSimulation(`Inst libssl3t64 [3.0.13-0ubuntu3.1] (3.0.13-0ubuntu3.4 Ubuntu:24.04/noble-updates [amd64])
Remv apache2 [2.4.58-1ubuntu8]
Purg oldpkg [1.0]
Inst local-thing (1.0 local [amd64])
Conf libssl3t64 (3.0.13-0ubuntu3.4 Ubuntu:24.04/noble-updates [amd64])`)
	if err != nil {
		t.Fatal(err)
	}
	want := []noderev.Change{
		{Action: "upgrade", Package: "libssl3t64", Version: "3.0.13-0ubuntu3.4", Origins: []string{"Ubuntu:24.04/noble-updates"}},
		{Action: "remove", Package: "apache2", Version: "2.4.58-1ubuntu8"},
		{Action: "remove", Package: "oldpkg", Version: "1.0"},
		{Action: "install", Package: "local-thing", Version: "1.0", Origins: []string{"local"}},
	}
	if fmt.Sprint(changes) != fmt.Sprint(want) {
		t.Fatalf("got %+v\nwant %+v", changes, want)
	}
	if _, err := ParseSimulation("Inst weird line without version"); err == nil {
		t.Fatal("accepted a line it does not understand")
	}
}

func TestOnlyFixedProgramsRun(t *testing.T) {
	if _, _, _, err := (ExecRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", "true"}, nil); err == nil {
		t.Fatal("ExecRunner ran /bin/sh")
	}
}

func TestExpiredCacheIsNotUsed(t *testing.T) {
	e := newEnv(t)
	e.os.installed["nginx"] = "1"
	e.os.services["nginx.service"] = &fakeService{exists: true, running: true}
	e.publish(1, nil, []noderev.Service{{Name: "nginx.service", Running: true, Repair: []string{noderev.ActionStart}}})
	e.round()
	e.cp.err = errors.New("connection refused")
	e.os.services["nginx.service"].running = false
	e.now = e.now.Add(61 * time.Minute) // the revision was valid for an hour
	rep, _ := e.a.Round(context.Background())
	if e.os.services["nginx.service"].running || rep.Serial != 0 {
		t.Fatalf("acted on an expired revision (serial %d)", rep.Serial)
	}
}
