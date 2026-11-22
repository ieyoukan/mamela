package linux

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// candidate is a version apt could install, and where it comes from.
type candidate struct {
	version string
	origins []string
}

// upgradeApt is apt-get on a Debian 13 node, as far as upgrades go: its installed packages and their
// candidates, what upgrading one also does (needs), its lists (one release file, whose time is
// listsAt), and dpkg. apt-get update makes the lists the time of the clock (now) or fails as told.
type upgradeApt struct {
	t         *testing.T
	root      string
	now       func() time.Time
	installed map[string]string
	cands     map[string]candidate
	needs     map[string][]string // package -> lines its upgrade adds ("Inst x (1 O:1/s [amd64])", "Remv y [1]")
	commands  []string
	// updateSaid is what an update says on its errors (exit 0 unless updateExit); locked: the next
	// updates find the lists locked.
	updateSaid string
	updateExit int
	locked     int
	// installFail makes the upgrade fail after the first package (a partial upgrade).
	installFail string
}

const trixieRelease = "deb.debian.org_debian_dists_trixie_InRelease"

func newUpgradeApt(t *testing.T, n *mnode, now func() time.Time) *upgradeApt {
	a := &upgradeApt{t: t, root: n.root, now: now,
		installed: map[string]string{"bind9-libs": "1:9.20.11-4", "bind9-host": "1:9.20.11-4", "base-files": "13.8+deb13u6", "curl": "8.14.1-2"},
		cands: map[string]candidate{
			"bind9-libs": {"1:9.20.15-1~deb13u1", []string{"Debian-Security:13/stable-security"}},
			"bind9-host": {"1:9.20.15-1~deb13u1", []string{"Debian-Security:13/stable-security", "Debian:13.6/stable"}},
			"base-files": {"13.8+deb13u8", []string{"Debian:13.6/stable"}},
			"curl":       {"8.14.1-2", []string{"Debian:13.6/stable"}},
		},
		needs: map[string][]string{}}
	n.a.System.Runner = a
	n.a.System.AllowedOrigins = []string{"Debian:", "Debian-Security:"}
	a.setLists(now().Add(-time.Hour))
	return a
}

// setLists gives apt's release file the time at.
func (a *upgradeApt) setLists(at time.Time) {
	p := filepath.Join(a.root, aptListsDir, trixieRelease)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("signed"), 0o644); err != nil {
		a.t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		a.t.Fatal(err)
	}
}

func (a *upgradeApt) inst(pkg, version string) string {
	c := a.cands[pkg]
	return fmt.Sprintf("Inst %s [%s] (%s %s [amd64])", pkg, a.installed[pkg], version, strings.Join(c.origins, ", "))
}

func (a *upgradeApt) Run(_ context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if program != progAptGet {
		return nil, nil, -1, fmt.Errorf("mamelet ran %s %v", program, args)
	}
	if !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") {
		return nil, nil, -1, fmt.Errorf("apt-get without DEBIAN_FRONTEND=noninteractive: %v", env)
	}
	sim := slices.Contains(args, "-s")
	if !sim {
		a.commands = append(a.commands, "apt-get "+strings.Join(args, " "))
	}
	head := "Reading package lists...\nBuilding dependency tree...\nReading state information...\n"
	switch {
	case slices.Contains(args, "update"):
		if a.locked > 0 {
			a.locked--
			return nil, []byte("E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4242 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/\n"), 100, nil
		}
		out := "Hit:1 http://deb.debian.org/debian trixie InRelease\nGet:2 http://security.debian.org/debian-security trixie-security InRelease [43.4 kB]\nReading package lists...\n"
		if a.updateSaid == "" {
			a.setLists(a.now())
		}
		return []byte(out), []byte(a.updateSaid), a.updateExit, nil
	case slices.Contains(args, "dist-upgrade"):
		if !sim {
			a.t.Fatal("mamelet ran dist-upgrade")
		}
		var lines []string
		for _, pkg := range slices.Sorted(maps.Keys(a.installed)) {
			if c, ok := a.cands[pkg]; ok && c.version != a.installed[pkg] {
				lines = append(lines, a.inst(pkg, c.version))
			}
		}
		return []byte(head + "Calculating upgrade...\n" + strings.Join(lines, "\n") + "\n"), nil, 0, nil
	case slices.Contains(args, "install") && slices.Contains(args, "--only-upgrade"):
		var lines, did []string
		for _, arg := range args[slices.Index(args, "--only-upgrade")+1:] {
			pkg, version, _ := strings.Cut(arg, "=")
			c, ok := a.cands[pkg]
			switch {
			case a.installed[pkg] == version:
				continue
			case !ok || c.version != version:
				return []byte(head), []byte(fmt.Sprintf("E: Version '%s' for '%s' was not found\n", version, pkg)), 100, nil
			}
			lines = append(lines, a.inst(pkg, version))
			lines = append(lines, a.needs[pkg]...)
			did = append(did, pkg)
		}
		if sim {
			return []byte(head + strings.Join(lines, "\n") + "\n"), nil, 0, nil
		}
		for i, pkg := range did {
			if a.installFail != "" && i == 1 {
				return []byte(head), []byte(a.installFail), 100, nil
			}
			a.installed[pkg] = a.cands[pkg].version
		}
		return []byte(head), nil, 0, nil
	}
	return nil, nil, -1, fmt.Errorf("apt-get %v", args)
}

func (a *upgradeApt) ran() []string { c := a.commands; a.commands = nil; return c }

// upgradesPart asks mamelet to work out the upgrades from origins, and lets it read the lists again.
func upgradesPart(up *AptUpgrade, origins ...string) Part {
	return Part{AptUpgrades: &AptUpgrades{Sets: [][]string{origins}, Refresh: true, Upgrade: up}}
}

func upgradesOf(t *testing.T, rep noderev.Report) AptUpgradesState {
	t.Helper()
	o := observedOf(t, rep)
	if o.AptUpgrades == nil {
		t.Fatalf("no upgrades in %s", rep.Ext[Name])
	}
	return *o.AptUpgrades
}

// upgradeNode is a mamelet on a Debian 13 node; now is the clock it tells the lists' age by (it takes
// the revisions the tests offer at mt0 whatever it is).
func upgradeNode(t *testing.T) (*mnode, *upgradeApt, *time.Time) {
	n := newNode(t)
	now := mt0
	clock := func() time.Time { return now }
	old := aptNow
	aptNow = clock
	t.Cleanup(func() { aptNow = old })
	return n, newUpgradeApt(t, n, clock), &now
}

// mamelet reports the pending upgrades of each set of origins, their change set, and how old the lists
// are; it reads nothing again while they are fresh.
func TestMameletReportsPendingUpgrades(t *testing.T) {
	n, apt, _ := upgradeNode(t)
	apt.needs["bind9-libs"] = []string{"Inst libuv1t64 (1.50.0-2 Debian:13.6/stable [amd64])"}
	n.offer(Part{AptUpgrades: &AptUpgrades{Sets: [][]string{{"Debian-Security:"}, {"Debian-Security:", "Debian:"}}, Refresh: true}}, nil)
	st := upgradesOf(t, n.round())
	if got := apt.ran(); len(got) != 0 {
		t.Errorf("ran %v with fresh lists", got)
	}
	if !st.ListsAt.Equal(mt0.Add(-time.Hour)) || !st.RefreshedAt.IsZero() || st.Error != "" || len(st.Sets) != 2 {
		t.Fatalf("%+v", st)
	}
	sec := st.set([]string{"Debian-Security:"})
	if sec == nil || !slices.Equal(sec.Packages, []string{"bind9-host=1:9.20.15-1~deb13u1", "bind9-libs=1:9.20.15-1~deb13u1"}) {
		t.Fatalf("security: %+v", sec)
	}
	var lines []string
	for _, c := range sec.Changes {
		lines = append(lines, c.Line())
	}
	if got := strings.Join(lines, "\n"); got != `upgrade bind9-host 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security, Debian:13.6/stable)
upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security)
install libuv1t64 1.50.0-2 (Debian:13.6/stable)` {
		t.Errorf("the change set:\n%s", got)
	}
	if _, d := aptChangeSet(sec.Changes); d != sec.Digest {
		t.Errorf("digest %s, of its lines %s", sec.Digest, d)
	}
	both := st.set([]string{"Debian-Security:", "Debian:"})
	if both == nil || len(both.Packages) != 3 || !slices.Contains(both.Packages, "base-files=13.8+deb13u8") {
		t.Errorf("security and point release: %+v", both)
	}
}

// Lists older than AptListsMaxAge are read again (apt-get update), once, and the round's report comes
// from the new ones; one that fails says why, and is not tried again within AptRefreshRetry; another
// apt holding the lists is not a failure. With no permission to read them (a protected node), nothing
// runs.
func TestMameletReadsOldListsAgain(t *testing.T) {
	n, apt, now := upgradeNode(t)
	apt.setLists(mt0.Add(-400 * 24 * time.Hour)) // a year old: apt-daily does not run in the container
	n.offer(upgradesPart(nil, "Debian-Security:"), nil)
	st := upgradesOf(t, n.round())
	if got := apt.ran(); !slices.Equal(got, []string{"apt-get -o DPkg::Lock::Timeout=300 update"}) {
		t.Fatalf("ran %v", got)
	}
	if !st.RefreshedAt.Equal(mt0) || !st.ListsAt.Equal(mt0) || st.RefreshFailed != "" {
		t.Fatalf("after the update: %+v", st)
	}
	n.round()
	if got := apt.ran(); len(got) != 0 {
		t.Errorf("fresh lists read again: %v", got)
	}

	// Thirteen hours on: the security archive cannot be fetched (apt exits 0, as it does).
	*now = mt0.Add(13 * time.Hour)
	apt.updateSaid = "W: Failed to fetch http://security.debian.org/debian-security/dists/trixie-security/InRelease  Temporary failure resolving 'security.debian.org'\n" +
		"W: Some index files failed to download. They have been ignored, or old ones used instead.\n"
	st = upgradesOf(t, n.round())
	if !strings.Contains(st.RefreshFailed, "Temporary failure resolving 'security.debian.org'") || !st.RefreshFailedAt.Equal(*now) || !st.RefreshedAt.Equal(mt0) {
		t.Fatalf("a failed update: %+v", st)
	}
	apt.ran()
	*now = now.Add(30 * time.Minute)
	n.round()
	if got := apt.ran(); len(got) != 0 {
		t.Errorf("tried again within the hour: %v", got)
	}
	*now = now.Add(31 * time.Minute)
	apt.updateSaid, apt.locked = "", 1
	st = upgradesOf(t, n.round())
	if got := apt.ran(); len(got) != 1 || st.RefreshFailed == "" {
		t.Errorf("a locked update: %v %+v", got, st)
	}
	st = upgradesOf(t, n.round())
	if got := apt.ran(); len(got) != 1 || st.RefreshFailed != "" || !st.RefreshedAt.Equal(*now) {
		t.Errorf("after the lock: %v %+v", got, st)
	}
}

// On a node the Control Plane must not change (its own host, observe-only), the part has no permission
// to read the lists again: nothing runs, and the report says how old they are.
func TestMameletReadsNothingAgainOnAProtectedNode(t *testing.T) {
	n, apt, _ := upgradeNode(t)
	apt.setLists(mt0.Add(-400 * 24 * time.Hour))
	p := upgradesPart(nil, "Debian-Security:")
	protectAptUpgrades(&p)
	n.offer(p, nil)
	st := upgradesOf(t, n.round())
	if got := apt.ran(); len(got) != 0 || len(st.Sets) != 1 || !st.ListsAt.Equal(mt0.Add(-400*24*time.Hour)) {
		t.Errorf("a protected node: ran %v, %+v", got, st)
	}
}

// The approved upgrade runs only for the approved change set: a change set that changed since the plan
// is refused and nothing runs; one that is the approved one is upgraded with the approved versions.
func TestMameletUpgradesOnlyTheApprovedChangeSet(t *testing.T) {
	n, apt, _ := upgradeNode(t)
	n.offer(upgradesPart(nil, "Debian-Security:"), nil)
	sec := upgradesOf(t, n.round()).set([]string{"Debian-Security:"})
	up := &AptUpgrade{Origins: []string{"Debian-Security:"}, Packages: sec.Packages, ChangeSet: sec.Digest,
		Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionUpgradePackages}}

	// Between the plan and the apply, upgrading bind9-libs comes to need a removal.
	apt.needs["bind9-libs"] = []string{"Remv libisc-export1105 [1:9.11.19+dfsg-2.1]"}
	n.offer(upgradesPart(up, "Debian-Security:"), nil)
	rep := n.round()
	if got := apt.ran(); len(got) != 0 {
		t.Fatalf("ran %v", got)
	}
	if r := rep.Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeRefused || !strings.Contains(r[0].Detail, "not the approved "+sec.Digest) {
		t.Fatalf("results %+v", r)
	}
	if st := upgradesOf(t, rep); !strings.Contains(st.Failed, "refused: upgrading would now change") {
		t.Errorf("the failure is not reported: %+v", st)
	}

	// Approved again, for the change set as it is now: mamelet's own configuration refuses a removal.
	sec = upgradesOf(t, rep).set([]string{"Debian-Security:"})
	up2 := &AptUpgrade{Origins: up.Origins, Packages: sec.Packages, ChangeSet: sec.Digest, Grant: noderev.Once{Operation: opB, Attempt: 1, Action: ActionUpgradePackages}}
	n.offer(upgradesPart(up2, "Debian-Security:"), nil)
	rep = n.round()
	if r := rep.Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeRefused || !strings.Contains(r[0].Detail, "never removes packages") || len(apt.ran()) != 0 {
		t.Fatalf("a removal: %+v", r)
	}

	// Without it, the approved set is upgraded, exactly.
	delete(apt.needs, "bind9-libs")
	up3 := &AptUpgrade{Origins: up.Origins, Packages: up.Packages, ChangeSet: up.ChangeSet, Grant: noderev.Once{Operation: opC, Attempt: 1, Action: ActionUpgradePackages}}
	n.offer(upgradesPart(up3, "Debian-Security:"), nil)
	rep = n.round()
	if r := rep.Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeDone {
		t.Fatalf("results %+v", r)
	}
	want := "apt-get -o DPkg::Lock::Timeout=300 -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install --only-upgrade " +
		"bind9-host=1:9.20.15-1~deb13u1 bind9-libs=1:9.20.15-1~deb13u1"
	if got := apt.ran(); !slices.Equal(got, []string{want}) {
		t.Fatalf("ran %v", got)
	}
	if apt.installed["bind9-libs"] != "1:9.20.15-1~deb13u1" || apt.installed["base-files"] != "13.8+deb13u6" {
		t.Errorf("installed %v", apt.installed)
	}
	// The same round reports nothing pending from the origins any more; the attempt is not made twice.
	if sec := upgradesOf(t, rep).set([]string{"Debian-Security:"}); len(sec.Changes) != 0 {
		t.Errorf("still pending: %+v", sec)
	}
	n.round()
	if got := apt.ran(); len(got) != 0 {
		t.Errorf("made twice: %v", got)
	}
}

// An upgrade that failed half-way: what it did stays, the attempt is recorded failed, and the next
// approval of the same (old) change set is refused, since the fresh one is smaller; an attempt whose
// work was done (mamelet stopped before it recorded it) finds nothing left and is done.
func TestAnInterruptedUpgrade(t *testing.T) {
	n, apt, _ := upgradeNode(t)
	n.offer(upgradesPart(nil, "Debian-Security:"), nil)
	sec := upgradesOf(t, n.round()).set([]string{"Debian-Security:"})
	once := func(op string) noderev.Once {
		return noderev.Once{Operation: op, Attempt: 1, Action: ActionUpgradePackages}
	}
	up := AptUpgrade{Origins: []string{"Debian-Security:"}, Packages: sec.Packages, ChangeSet: sec.Digest, Grant: once(opA)}
	apt.installFail = "E: Sub-process /usr/bin/dpkg returned an error code (1)\n"
	n.offer(upgradesPart(&up, "Debian-Security:"), nil)
	rep := n.round()
	if r := rep.Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeFailed || !strings.Contains(r[0].Detail, "dpkg returned an error code") {
		t.Fatalf("results %+v", r)
	}
	if apt.installed["bind9-host"] != "1:9.20.15-1~deb13u1" || apt.installed["bind9-libs"] != "1:9.20.11-4" {
		t.Fatalf("installed %v", apt.installed)
	}
	left := upgradesOf(t, rep).set([]string{"Debian-Security:"})
	if !slices.Equal(left.Packages, []string{"bind9-libs=1:9.20.15-1~deb13u1"}) {
		t.Fatalf("what is left: %+v", left)
	}
	apt.installFail = ""
	retry := up
	retry.Grant = noderev.Once{Operation: opA, Attempt: 2, Action: ActionUpgradePackages}
	n.offer(upgradesPart(&retry, "Debian-Security:"), nil)
	if r := n.round().Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeRefused || !strings.Contains(r[0].Detail, "plan again") {
		t.Fatalf("a retry of the old change set: %+v", r)
	}

	// The upgrade happened (by hand, or by an attempt mamelet did not record): done, nothing runs.
	apt.installed["bind9-libs"] = "1:9.20.15-1~deb13u1"
	apt.ran()
	again := up
	again.Grant = once(opB)
	n.offer(upgradesPart(&again, "Debian-Security:"), nil)
	if r := n.round().Results; len(r) != 1 || r[0].Outcome != noderev.OutcomeDone || !strings.Contains(r[0].Detail, "already") || len(apt.ran()) != 0 {
		t.Fatalf("already upgraded: %+v", r)
	}
}

// dpkg's journal, /run/reboot-required and its packages are reported as they are.
func TestMameletReportsDpkgAndReboot(t *testing.T) {
	n, _, _ := upgradeNode(t)
	write(t, filepath.Join(n.root, "var/lib/dpkg/updates/0003"), "Package: bind9-libs\n")
	write(t, filepath.Join(n.root, "run/reboot-required"), "*** System restart required ***\n")
	write(t, filepath.Join(n.root, "run/reboot-required.pkgs"), "linux-image-6.12.57+deb13-amd64\nlibc6\nlibc6\n$(reboot)\n")
	n.offer(upgradesPart(nil, "Debian-Security:"), nil)
	st := upgradesOf(t, n.round())
	if !st.DpkgInterrupted || !st.RebootRequired || !slices.Equal(st.RebootPackages, []string{"libc6", "linux-image-6.12.57+deb13-amd64"}) {
		t.Errorf("%+v", st)
	}
	os.Remove(filepath.Join(n.root, "var/lib/dpkg/updates/0003"))
	write(t, filepath.Join(n.root, "var/lib/dpkg/updates/tmp.i"), "")
	if st := upgradesOf(t, n.round()); st.DpkgInterrupted {
		t.Error("dpkg's temporary file taken for an interruption")
	}
}
