package k3s

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

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// A copy of mamelet's fakes (internal/mamelet/agent_test.go), to run mamelet's rounds with k3s.

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
	// started is when a service's main process started (unix seconds), as systemd says; restart
	// sets it to restartedAt. kubectl is what `k3s kubectl get nodes` prints.
	started     map[string]int64
	restartedAt int64
	kubectl     string
	// nextRuns: the staged mamelet runs with -version (and prints nextVersion).
	nextRuns    bool
	nextVersion string
	// kubectlFails names a kubectl verb (drain, delete) that exits 1.
	kubectlFails string
}

func newFakeOS() *fakeOS {
	return &fakeOS{installed: map[string]string{}, sim: map[string]string{"nginx": nginxSim},
		services: map[string]*fakeService{}, provides: map[string]string{"nginx": "nginx.service"}, started: map[string]int64{}}
}

func (f *fakeOS) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	f.commands = append(f.commands, program+" "+strings.Join(args, " "))
	if strings.HasSuffix(program, "mamelet.next") {
		if f.nextRuns {
			return []byte(f.nextVersion + "\n"), nil, 0, nil
		}
		return nil, []byte("exec format error"), 126, nil
	}
	last := args[len(args)-1]
	switch program {
	case "/usr/bin/dpkg-query":
		if v, ok := f.installed[last]; ok {
			return []byte("installed\t" + v + "\n"), nil, 0, nil
		}
		return nil, []byte("dpkg-query: no packages found matching " + last), 1, nil
	case "/usr/bin/apt-get":
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
	case progK3s:
		switch verb := args[1]; {
		case verb == f.kubectlFails:
			return nil, []byte("error: " + verb + " failed"), 1, nil
		case verb == "delete": // kubectl delete node <name>
			var keep []string
			for _, l := range strings.Split(f.kubectl, "\n") {
				if !strings.HasPrefix(l, last+"\t") && l != "" {
					keep = append(keep, l)
				}
			}
			f.kubectl = strings.Join(keep, "\n") + "\n"
			return nil, nil, 0, nil
		case verb == "get":
			return []byte(f.kubectl), nil, 0, nil
		}
		return nil, nil, 0, nil // cordon, drain, uncordon
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
			out := fmt.Sprintf("LoadState=%s\nUnitFileState=%s\nActiveState=%s\n", load, unit, active)
			if t := f.started[last]; t > 0 && s.running {
				out += fmt.Sprintf("ExecMainStartTimestamp=@%d\n", t)
			}
			return []byte(out), nil, 0, nil
		case "enable":
			s.enabled = true
		case "start":
			s.running = true
		case "restart":
			s.running = true
			if f.started != nil {
				f.started[last] = f.restartedAt
			}
		case "disable":
			s.enabled = false
		case "stop":
			s.running = false
		}
		return nil, nil, 0, nil
	}
	return nil, nil, -1, fmt.Errorf("unexpected program %s", program)
}

func (f *fakeOS) changes() []string {
	var out []string
	for _, c := range f.commands {
		if (strings.HasPrefix(c, "/usr/bin/apt-get") && !strings.Contains(c, " -s ")) ||
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
	// binaries is what the Control Plane serves as mamelet, by the sha256 asked for.
	binaries map[string][]byte
	fetched  []string
}

func (c *fakeCP) Mamelet(_ context.Context, sha string) ([]byte, error) {
	c.fetched = append(c.fetched, sha)
	b, ok := c.binaries[sha]
	if !ok {
		return nil, errors.New("the Control Plane does not serve a mamelet with this sha256")
	}
	return b, nil
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
	a   *mamelet.Agent

	k3sBody string // the binary the fake release server serves
}

func newEnv(t *testing.T) *env {
	seed := make([]byte, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	e := &env{t: t, key: key, os: newFakeOS(), cp: &fakeCP{}, now: t0}
	e.a = &mamelet.Agent{
		Config: mamelet.Config{Node: "web", VMID: 7001, RevisionKey: key.Public().(ed25519.PublicKey), AllowedOrigins: []string{"Ubuntu:"}},
		CP:     e.cp, System: &mamelet.System{Runner: e.os, AllowedOrigins: []string{"Ubuntu:"}},
		State: mamelet.FileState{Path: filepath.Join(t.TempDir(), "state.json")}, Now: func() time.Time { return e.now }, Version: "test",
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
	changes, err := mamelet.ParseSimulation(nginxSim)
	if err != nil {
		t.Fatal(err)
	}
	return noderev.NewChangeSet(changes)
}
