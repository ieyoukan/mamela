package mamelet

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// Config is mamelet's configuration, written by cloud-init (/etc/mamelet/config.json).
type Config struct {
	Node            string   `json:"node"`
	VMID            int      `json:"vmid"`
	Server          string   `json:"server"`          // the Control Plane, e.g. https://127.0.0.1:8443
	CA              string   `json:"ca"`              // PEM of the Control Plane's CA
	RevisionKey     []byte   `json:"revisionKey"`     // Ed25519 public key that signs revisions
	EnrollTokenFile string   `json:"enrollTokenFile"` // one-time token; deleted after enrolling
	StateDir        string   `json:"stateDir"`
	IntervalSeconds int      `json:"intervalSeconds"`
	AllowedOrigins  []string `json:"allowedOrigins"` // APT origins packages may come from
}

// LoadConfig reads and checks the configuration.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case !noderev.ValidNode(c.Node) || c.VMID <= 0:
		return Config{}, fmt.Errorf("%s: node %q / vmid %d are not valid", path, c.Node, c.VMID)
	case c.Server == "" || c.CA == "" || len(c.RevisionKey) != ed25519.PublicKeySize || c.StateDir == "":
		return Config{}, fmt.Errorf("%s: server, ca, revisionKey and stateDir are required", path)
	case len(c.AllowedOrigins) == 0:
		return Config{}, fmt.Errorf("%s: allowedOrigins must name at least one APT origin", path)
	}
	if c.IntervalSeconds <= 0 {
		c.IntervalSeconds = 30
	}
	return c, nil
}

// ControlPlane is how mamelet talks to the Control Plane (Client, or a fake in tests).
type ControlPlane interface {
	Revision(ctx context.Context) (noderev.Signed, error)
	Report(ctx context.Context, r noderev.Report) error
}

// State is what mamelet remembers between rounds and restarts.
type State struct {
	LastSerial int64           `json:"lastSerial"`
	Revision   *noderev.Signed `json:"revision,omitempty"` // the last accepted revision
	// Attempts holds the outcome of each one-time operation attempt ("<operation>#<attempt>"):
	// an attempt is never made twice.
	Attempts map[string]noderev.Result `json:"attempts"`
}

// StateStore keeps the state.
type StateStore interface {
	Load() (State, error)
	Save(State) error
}

// FileState keeps the state in a JSON file, replaced atomically.
type FileState struct{ Path string }

func (f FileState) Load() (State, error) {
	st := State{Attempts: map[string]noderev.Result{}}
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st, json.RejectUnknownMembers(true)); err != nil {
		return st, fmt.Errorf("%s: %w", f.Path, err)
	}
	if st.Attempts == nil {
		st.Attempts = map[string]noderev.Result{}
	}
	return st, nil
}

func (f FileState) Save(st State) error {
	data, err := json.Marshal(st, json.Deterministic(true))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

// Agent is one mamelet.
type Agent struct {
	Config  Config
	CP      ControlPlane
	System  *System
	State   StateStore
	Now     func() time.Time
	Version string
}

// Round fetches the revision, observes, makes the allowed changes and reports. The returned
// report is the one sent (or that would have been sent, if the Control Plane is unreachable).
func (a *Agent) Round(ctx context.Context) (noderev.Report, error) {
	st, err := a.State.Load()
	if err != nil {
		return noderev.Report{}, err
	}
	rep := noderev.Report{Node: a.Config.Node, VMID: a.Config.VMID, Version: a.Version,
		Packages: []noderev.PackageState{}, Services: []noderev.ServiceState{}, Results: []noderev.Result{}}
	key := ed25519.PublicKey(a.Config.RevisionKey)
	expect := func() noderev.Expectation {
		return noderev.Expectation{Node: a.Config.Node, VMID: a.Config.VMID, Now: a.Now(), LastSerial: st.LastSerial}
	}

	var rev *noderev.Revision
	online := false
	signed, fetchErr := a.CP.Revision(ctx)
	if fetchErr == nil {
		r, err := noderev.Verify(key, signed, expect())
		if err != nil {
			rep.Refused = err.Error()
		} else {
			rev, online = &r, true
			st.LastSerial, st.Revision = r.Serial, &signed
		}
	}
	if rev == nil && st.Revision != nil {
		// Offline, or the new revision was refused: the last accepted one, if still valid, allows
		// only standing repairs (one-time operations need a fresh revision).
		if r, err := noderev.Verify(key, *st.Revision, expect()); err == nil {
			rev = &r
		}
	}

	if rev != nil {
		rep.Serial = rev.Serial
		pkgs, svcs := a.observe(ctx, rev)
		if a.act(ctx, rev, online, pkgs, svcs, &st, &rep) {
			pkgs, svcs = a.observe(ctx, rev)
		}
		rep.Packages, rep.Services = pkgs, svcs
		rep.Results = append(rep.Results, a.knownResults(rev, st)...)
	}
	if err := a.State.Save(st); err != nil {
		return rep, fmt.Errorf("save state: %w", err)
	}
	if fetchErr != nil {
		return rep, fmt.Errorf("fetch revision: %w", fetchErr)
	}
	if err := a.CP.Report(ctx, rep); err != nil {
		return rep, fmt.Errorf("send report: %w", err)
	}
	return rep, nil
}

func (a *Agent) observe(ctx context.Context, rev *noderev.Revision) ([]noderev.PackageState, []noderev.ServiceState) {
	pkgs := []noderev.PackageState{}
	for _, p := range rev.Packages {
		ps := noderev.PackageState{Name: p.Name}
		installed, ver, err := a.System.Package(ctx, p.Name)
		switch {
		case err != nil:
			ps.Error = err.Error()
		case installed:
			ps.Installed, ps.Version = true, ver
		default:
			cs, err := a.System.Simulate(ctx, p.Name)
			if err != nil {
				ps.Error = "simulate: " + err.Error()
			} else {
				ps.ChangeSet = &cs
			}
		}
		pkgs = append(pkgs, ps)
	}
	svcs := []noderev.ServiceState{}
	for _, s := range rev.Services {
		ss, err := a.System.Service(ctx, s.Name)
		if err != nil {
			ss = noderev.ServiceState{Name: s.Name}
		}
		svcs = append(svcs, ss)
	}
	return pkgs, svcs
}

func attemptKey(op string, attempt int) string { return op + "#" + strconv.Itoa(attempt) }

// act makes the changes the revision allows. One-time operations need a revision fetched this
// round (online); standing repairs may use a still-valid cached one. It reports whether it ran
// any command that changes the system.
func (a *Agent) act(ctx context.Context, rev *noderev.Revision, online bool, pkgs []noderev.PackageState,
	svcs []noderev.ServiceState, st *State, rep *noderev.Report) bool {
	changed := false
	record := func(op string, attempt int, outcome, detail string) {
		r := noderev.Result{Operation: op, Attempt: attempt, Outcome: outcome, Detail: detail}
		st.Attempts[attemptKey(op, attempt)] = r
	}

	for i, p := range rev.Packages {
		in := p.Install
		if in == nil || !online || pkgs[i].Installed || pkgs[i].Error != "" {
			continue
		}
		if _, done := st.Attempts[attemptKey(in.Operation, in.Attempt)]; done {
			continue
		}
		cs := pkgs[i].ChangeSet
		switch {
		case cs == nil:
			record(in.Operation, in.Attempt, noderev.OutcomeFailed, "the installation could not be simulated")
		case cs.Digest != in.ChangeSet:
			record(in.Operation, in.Attempt, noderev.OutcomeRefused, fmt.Sprintf(
				"installing %s would now change %s, not the approved %s; plan again", p.Name, cs.Digest, in.ChangeSet))
		case a.System.Refusal(*cs) != "":
			record(in.Operation, in.Attempt, noderev.OutcomeRefused, a.System.Refusal(*cs))
		default:
			changed = true
			if err := a.System.Install(ctx, p.Name); err != nil {
				record(in.Operation, in.Attempt, noderev.OutcomeFailed, err.Error())
			} else {
				record(in.Operation, in.Attempt, noderev.OutcomeDone, "installed "+p.Name)
			}
		}
	}

	for i, s := range rev.Services {
		obs := svcs[i]
		if !obs.Exists {
			continue // not installed yet: nothing to enable or start
		}
		if s.Enabled && !obs.Enabled {
			if o, ok := a.pendingOnce(s, noderev.ActionEnable, online, st); ok {
				changed = true
				if err := a.System.Enable(ctx, s.Name); err != nil {
					record(o.Operation, o.Attempt, noderev.OutcomeFailed, err.Error())
				} else {
					record(o.Operation, o.Attempt, noderev.OutcomeDone, "enabled "+s.Name)
				}
			}
		}
		if s.Running && !obs.Running {
			if slices.Contains(s.Repair, noderev.ActionStart) {
				changed = true
				r := noderev.Result{Operation: "repair", Outcome: noderev.OutcomeDone, Detail: "started " + s.Name + " (drift repair)"}
				if err := a.System.Start(ctx, s.Name); err != nil {
					r.Outcome, r.Detail = noderev.OutcomeFailed, "start "+s.Name+": "+err.Error()
				}
				rep.Results = append(rep.Results, r)
			} else if o, ok := a.pendingOnce(s, noderev.ActionStart, online, st); ok {
				changed = true
				if err := a.System.Start(ctx, s.Name); err != nil {
					record(o.Operation, o.Attempt, noderev.OutcomeFailed, err.Error())
				} else {
					record(o.Operation, o.Attempt, noderev.OutcomeDone, "started "+s.Name)
				}
			}
		}
	}
	return changed
}

func (a *Agent) pendingOnce(s noderev.Service, action string, online bool, st *State) (noderev.Once, bool) {
	if !online {
		return noderev.Once{}, false
	}
	for _, o := range s.Once {
		if o.Action != action {
			continue
		}
		if _, done := st.Attempts[attemptKey(o.Operation, o.Attempt)]; !done {
			return o, true
		}
	}
	return noderev.Once{}, false
}

// knownResults are the recorded outcomes of the one-time operations in the revision, so the
// Control Plane learns them even if an earlier report was lost.
func (a *Agent) knownResults(rev *noderev.Revision, st State) []noderev.Result {
	var out []noderev.Result
	for _, p := range rev.Packages {
		if p.Install != nil {
			if r, ok := st.Attempts[attemptKey(p.Install.Operation, p.Install.Attempt)]; ok {
				out = append(out, r)
			}
		}
	}
	for _, s := range rev.Services {
		for _, o := range s.Once {
			if r, ok := st.Attempts[attemptKey(o.Operation, o.Attempt)]; ok {
				out = append(out, r)
			}
		}
	}
	return out
}
