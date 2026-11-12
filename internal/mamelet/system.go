// Package mamelet is the agent that runs in each VM. It pulls a signed revision from the Control
// Plane, observes APT packages and systemd services, makes only the changes the revision allows,
// and reports back. It never runs a shell: every command is a fixed program with arguments that
// were checked against narrow patterns (noderev).
package mamelet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// Runner runs one program. Tests replace it; the real one is ExecRunner.
type Runner interface {
	Run(ctx context.Context, program string, args []string, env []string) (stdout, stderr []byte, exitCode int, err error)
}

// The only programs mamelet runs.
const (
	progDpkgQuery = "/usr/bin/dpkg-query"
	progAptGet    = "/usr/bin/apt-get"
	progSystemctl = "/usr/bin/systemctl"
)

// ExecRunner runs programs directly (no shell), with a minimal environment.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	switch program {
	case progDpkgQuery, progAptGet, progSystemctl:
	default:
		return nil, nil, -1, fmt.Errorf("mamelet does not run %s", program)
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append([]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.Bytes(), errOut.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return out.Bytes(), errOut.Bytes(), -1, err
	}
	return out.Bytes(), errOut.Bytes(), 0, nil
}

// System is the OS as mamelet sees it.
type System struct {
	Runner Runner
	// AllowedOrigins are the APT origins packages may come from (prefixes, e.g. "Ubuntu:").
	AllowedOrigins []string
	// Root prefixes the files mamelet writes (k3s); empty is "/". Tests use a directory.
	Root string
	// HTTP downloads k3s (nil: a client with a timeout); K3sReleases overrides where from (tests).
	HTTP        *http.Client
	K3sReleases string
	// Addresses overrides how the node's IPv4 addresses are read (tests).
	Addresses func() []string
}

func (s *System) run(ctx context.Context, program string, args []string, env ...string) (string, error) {
	out, errOut, code, err := s.Runner.Run(ctx, program, args, env)
	if err != nil {
		return "", err
	}
	if code != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return string(out), fmt.Errorf("%s %s: exit %d: %s", program, strings.Join(args, " "), code, lastLines(msg, 5))
	}
	return string(out), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Package observes one package: installed (fully) and its version.
func (s *System) Package(ctx context.Context, name string) (installed bool, version string, err error) {
	if !noderev.ValidPackage(name) {
		return false, "", fmt.Errorf("invalid package name %q", name)
	}
	out, errOut, code, err := s.Runner.Run(ctx, progDpkgQuery, []string{"-W", "-f=${db:Status-Status}\t${Version}\n", name}, nil)
	if err != nil {
		return false, "", err
	}
	if code != 0 {
		if strings.Contains(string(errOut), "no packages found") {
			return false, "", nil
		}
		return false, "", fmt.Errorf("dpkg-query %s: exit %d: %s", name, code, strings.TrimSpace(string(errOut)))
	}
	status, ver, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if status != "installed" {
		return false, "", nil
	}
	return true, ver, nil
}

// aptOptions are used for both the simulation and the installation, so the two compute the same
// change set. Recommends are installed as Debian does by default; the change set shows them.
var aptOptions = []string{"-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// Simulate computes what installing a package would change (apt-get -s). It changes nothing.
func (s *System) Simulate(ctx context.Context, name string) (noderev.ChangeSet, error) {
	if !noderev.ValidPackage(name) {
		return noderev.ChangeSet{}, fmt.Errorf("invalid package name %q", name)
	}
	args := append([]string{"-s", "-o", "Debug::NoLocking=1"}, aptOptions...)
	out, err := s.run(ctx, progAptGet, append(args, "install", name), "DEBIAN_FRONTEND=noninteractive")
	if err != nil {
		return noderev.ChangeSet{}, err
	}
	changes, err := ParseSimulation(out)
	if err != nil {
		return noderev.ChangeSet{}, err
	}
	return noderev.NewChangeSet(changes), nil
}

// Install installs a package. The caller has checked the change set just before.
func (s *System) Install(ctx context.Context, name string) error {
	if !noderev.ValidPackage(name) {
		return fmt.Errorf("invalid package name %q", name)
	}
	args := append([]string{"-o", "DPkg::Lock::Timeout=300"}, aptOptions...)
	_, err := s.run(ctx, progAptGet, append(args, "install", name), "DEBIAN_FRONTEND=noninteractive")
	return err
}

// Refusal says why a change set may not be installed by mamelet, whatever the revision says.
func (s *System) Refusal(cs noderev.ChangeSet) string {
	if cs.Removes() {
		return "installing would remove packages; mamelet never removes packages"
	}
	for _, c := range cs.Changes {
		if len(c.Origins) == 0 {
			return fmt.Sprintf("%s %s has no known origin", c.Package, c.Version)
		}
		for _, o := range c.Origins {
			if !s.allowedOrigin(o) {
				return fmt.Sprintf("%s %s comes from %s, which is not an allowed APT origin (%s)", c.Package, c.Version, o,
					strings.Join(s.AllowedOrigins, ", "))
			}
		}
	}
	return ""
}

func (s *System) allowedOrigin(o string) bool {
	for _, p := range s.AllowedOrigins {
		if p != "" && strings.HasPrefix(o, p) {
			return true
		}
	}
	return false
}

var simLine = regexp.MustCompile(`^(Inst|Remv|Purg) (\S+)(?: \[([^\]]*)\])?(?: \((.*)\))?$`)

// ParseSimulation reads the Inst/Remv/Purg lines of apt-get -s output.
func ParseSimulation(out string) ([]noderev.Change, error) {
	var changes []noderev.Change
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") && !strings.HasPrefix(line, "Remv ") && !strings.HasPrefix(line, "Purg ") {
			continue
		}
		m := simLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("unexpected apt-get -s line: %q", line)
		}
		c := noderev.Change{Package: m[2]}
		switch {
		case m[1] == "Remv" || m[1] == "Purg":
			c.Action, c.Version = noderev.ChangeRemove, m[3]
		case m[3] != "":
			c.Action = noderev.ChangeUpgrade
		default:
			c.Action = noderev.ChangeInstall
		}
		if m[1] == "Inst" {
			inside := m[4]
			if i := strings.LastIndex(inside, " ["); i >= 0 {
				inside = inside[:i]
			}
			ver, rest, _ := strings.Cut(inside, " ")
			c.Version = ver
			for _, o := range strings.Split(rest, ",") {
				if o = strings.TrimSpace(o); o != "" {
					c.Origins = append(c.Origins, o)
				}
			}
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// Service observes a systemd service.
func (s *System) Service(ctx context.Context, name string) (noderev.ServiceState, error) {
	st := noderev.ServiceState{Name: name}
	if !noderev.ValidService(name) {
		return st, fmt.Errorf("invalid service name %q", name)
	}
	out, err := s.run(ctx, progSystemctl, []string{"show", "--property=LoadState,UnitFileState,ActiveState", "--", name})
	if err != nil {
		return st, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	st.Exists = props["LoadState"] == "loaded"
	st.Enabled = props["UnitFileState"] == "enabled"
	st.Running = props["ActiveState"] == "active"
	return st, nil
}

// Enable enables a service (for the next boot; it does not start it).
func (s *System) Enable(ctx context.Context, name string) error {
	if !noderev.ValidService(name) {
		return fmt.Errorf("invalid service name %q", name)
	}
	_, err := s.run(ctx, progSystemctl, []string{"enable", "--", name})
	return err
}

// StartTimeout bounds how long mamelet waits for a service to start. A service that waits for
// something else (a k3s agent for its server, with Type=notify) would otherwise hold mamelet, and
// with it every report, for as long as that takes. systemd keeps starting it after the deadline.
var StartTimeout = 2 * time.Minute

// Start starts a service.
func (s *System) Start(ctx context.Context, name string) error {
	if !noderev.ValidService(name) {
		return fmt.Errorf("invalid service name %q", name)
	}
	sctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	_, err := s.run(sctx, progSystemctl, []string{"start", "--", name})
	if err != nil && sctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("%s did not finish starting within %s (systemd keeps starting it; the next observation shows whether it runs)", name, StartTimeout)
	}
	return err
}

// Disable disables a service (for the next boot; it does not stop it).
func (s *System) Disable(ctx context.Context, name string) error {
	if !noderev.ValidService(name) {
		return fmt.Errorf("invalid service name %q", name)
	}
	_, err := s.run(ctx, progSystemctl, []string{"disable", "--", name})
	return err
}

// Stop stops a service.
func (s *System) Stop(ctx context.Context, name string) error {
	if !noderev.ValidService(name) {
		return fmt.Errorf("invalid service name %q", name)
	}
	_, err := s.run(ctx, progSystemctl, []string{"stop", "--", name})
	return err
}
