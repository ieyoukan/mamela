package linux

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// What mamelet does for NodeAptUpgrades. It runs only apt-get, the core's program, with fixed options:
//
//   - to work out what is pending, two simulations (-s, without apt's lock): apt-get dist-upgrade, whose
//     Inst lines name every installed package that has a candidate of a higher version and where the
//     candidate comes from; then apt-get install --only-upgrade of the packages whose candidate comes
//     from one of the declared origins, pinned to that candidate (<package>=<version>), whose lines are
//     the change set the plan shows (with the new packages they need, and any removal, which the
//     policy denies);
//   - to upgrade, the same apt-get install --only-upgrade of the approved packages and versions, with
//     dpkg's lock timeout, once a simulation just before gives the approved change set again;
//   - to keep its view fresh, apt-get update when the package lists are older than AptListsMaxAge.
//
// Reading the lists again is the one thing mamelet does without an approval, and only where the part
// lets it (a node the Control Plane may change, whose declarations planned upgrades): it is what
// Debian's and Ubuntu's apt-daily does twice a day. It changes what apt knows is available (the files
// of /var/lib/apt/lists), never what is installed, and runs nothing the node's own configuration does
// not run on any apt-get update (its APT::Update hooks, which no Mamela kind writes: CheckPath keeps
// /etc/apt to NodeAptRepo). Without it, a node whose apt-daily does not run (an unprivileged container,
// where it waits for a network systemd-networkd does not manage, and gives up) would never see a
// security update. Nothing is upgraded, and no change set of an installation is approved, without a
// person's approval of exactly what apt would do.

// Bounds of what mamelet waits for, and how often it reads the lists again.
var (
	// AptRefreshRetry: after an apt-get update that failed, mamelet waits this long before it tries
	// again (a network that is down is not asked every round).
	AptRefreshRetry = time.Hour
	// AptSimulateTimeout bounds one simulation.
	AptSimulateTimeout = 5 * time.Minute
	// AptUpgradeTimeout bounds an upgrade. It is not cut short when mamelet is asked to stop (dpkg
	// killed in the middle leaves packages half-configured): it ends by itself, or at this bound.
	AptUpgradeTimeout = 30 * time.Minute
	// aptNow is mamelet's clock for the lists' age (tests set it).
	aptNow = time.Now
)

// refreshName is mamelet's record of its own apt-get update runs, in its state directory.
const refreshName = "linux-apt-refresh.json"

// refreshRecord is when mamelet's apt-get update last read every source, and when it last failed and
// why. A record that cannot be read is taken as empty: the lists are then read again once more.
type refreshRecord struct {
	Succeeded time.Time `json:"succeeded,omitzero"`
	Failed    string    `json:"failed,omitempty"`
	FailedAt  time.Time `json:"failedAt,omitzero"`
}

func loadRefresh(dir string) refreshRecord {
	var rec refreshRecord
	if dir == "" {
		return rec
	}
	data, err := readRecord(dir, refreshName)
	if err != nil || decodeRecord(data, &rec) != nil {
		return refreshRecord{}
	}
	return rec
}

// aptSimArgs are the simulation's options: apt-get -s without apt's lock (it changes nothing), with the
// core's options (the installation uses the same ones, so that both compute the same change set).
var aptSimArgs = []string{"-s", "-o", "Debug::NoLocking=1", "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// aptUpgradeArgs are the upgrade's options: dpkg's lock is waited for, as the core's installation does.
var aptUpgradeArgs = []string{"-o", "DPkg::Lock::Timeout=300", "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// observeAptUpgrades reports how old the package lists are, whether dpkg was interrupted and the node
// needs a reboot, and for each set of origins of the part, the pending upgrades and their change set.
func observeAptUpgrades(ctx context.Context, s *mamelet.System, p Part, o *Observed) {
	u := p.AptUpgrades
	if u == nil {
		return
	}
	st := &AptUpgradesState{}
	o.AptUpgrades = st
	r, err := openTree(s)
	if err != nil {
		st.Error = err.Error()
		return
	}
	defer r.Close()
	st.ListsAt = newestRelease(r)
	rec := loadRefresh(stateDir(s))
	st.RefreshedAt = rec.Succeeded
	if rec.Failed != "" && rec.FailedAt.After(rec.Succeeded) {
		st.RefreshFailed, st.RefreshFailedAt = rec.Failed, rec.FailedAt
	}
	st.DpkgInterrupted = dpkgInterrupted(r)
	st.RebootRequired, st.RebootPackages = rebootRequired(r)
	if len(u.Sets) == 0 {
		return
	}
	pending, err := simulate(ctx, s, "dist-upgrade")
	if err != nil {
		st.Error = "apt-get -s dist-upgrade: " + bounded(err.Error(), maxAptDetail)
		return
	}
	for i, origins := range u.Sets {
		set := AptUpgradeSet{Origins: origins}
		switch {
		case i >= maxAptUpgradeSets:
			set.Error = fmt.Sprintf("mamelet works out the upgrades of at most %d sets of origins on one node, and the revision names more: this one was left out (the Control Plane and mamelet are of different releases?)", maxAptUpgradeSets)
		default:
			if m, _ := aptOriginsProblem(origins); m != "" {
				set.Error = "mamelet refuses the origins: " + m
				break
			}
			pins := upgradesFrom(pending, origins)
			if len(pins) > MaxAptUpgrades {
				set.Error = fmt.Sprintf("%d packages are to be upgraded from these origins; mamelet upgrades at most %d at once (that many is a release upgrade: do it by hand)", len(pins), MaxAptUpgrades)
				break
			}
			if len(pins) == 0 {
				break
			}
			changes, err := simulate(ctx, s, "install", slices.Concat([]string{"--only-upgrade"}, pins)...)
			if err != nil {
				set.Error = "apt-get -s install --only-upgrade: " + bounded(err.Error(), maxAptDetail)
				break
			}
			set.Packages = pins
			set.Changes, set.Digest = aptChangeSet(changes)
		}
		st.Sets = append(st.Sets, set)
	}
}

// upgradesFrom are the packages of a simulation's upgrades whose candidate comes from one of origins,
// each pinned to it ("<package>=<version>"), sorted.
func upgradesFrom(sim []AptChange, origins []string) []string {
	var out []string
	for _, c := range sim {
		if c.Action != noderev.ChangeUpgrade || !slices.ContainsFunc(c.Origins, func(o string) bool { return matchesOrigin(origins, o) }) {
			continue
		}
		if pin := c.Package + "=" + c.Version; !slices.Contains(out, pin) {
			out = append(out, pin)
		}
	}
	slices.Sort(out)
	return out
}

// simulate runs apt-get -s <command> [args] and reads its lines (parseAptSimulation).
func simulate(ctx context.Context, s *mamelet.System, command string, args ...string) ([]AptChange, error) {
	for _, a := range args {
		if _, _, ok := splitPin(a); !ok && a != "--only-upgrade" {
			return nil, fmt.Errorf("%q is not a package and version mamelet passes to apt-get", a)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, AptSimulateTimeout)
	defer cancel()
	out, err := s.Run(sctx, progAptGet, slices.Concat(aptSimArgs, []string{command}, args), "DEBIAN_FRONTEND=noninteractive")
	if err != nil {
		return nil, err
	}
	return parseAptSimulation(out)
}

// simLineRe is a line of apt-get -s that changes a package (pkgSimulate): "Inst <package> [<installed>]
// (<candidate> <origin>, <origin> [<arch>])", the [installed] left out for a new package, "Remv
// <package> [<installed>]" or "Purg ...", each followed by the packages the simulation finds broken at
// that step ("[libc6:i386 ]", "[a on b]"), which say nothing of the change.
var simLineRe = regexp.MustCompile(`^(Inst|Remv|Purg) (\S+)(?: \[([^\]]*)\])?(?: \(([^()]*)\))?((?: \[[^\]]*\])*)$`)

// parseAptSimulation reads the Inst, Remv and Purg lines of apt-get -s (Conf lines and everything else
// say nothing more). A line of that kind it cannot read is an error: a change set is complete or it is
// not one.
func parseAptSimulation(out string) ([]AptChange, error) {
	var changes []AptChange
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") && !strings.HasPrefix(line, "Remv ") && !strings.HasPrefix(line, "Purg ") {
			continue
		}
		m := simLineRe.FindStringSubmatch(line)
		if m == nil || !aptQualifiedRe.MatchString(m[2]) {
			return nil, fmt.Errorf("unexpected apt-get -s line: %q", bounded(line, 200))
		}
		c := AptChange{Package: m[2], From: m[3]}
		switch {
		case m[1] != "Inst":
			c.Action = noderev.ChangeRemove
			if m[4] != "" {
				return nil, fmt.Errorf("unexpected apt-get -s line: %q", bounded(line, 200))
			}
		case m[4] == "":
			return nil, fmt.Errorf("apt-get -s line without the version it installs: %q", bounded(line, 200))
		case m[3] != "":
			c.Action = noderev.ChangeUpgrade
		default:
			c.Action = noderev.ChangeInstall
		}
		if m[4] != "" {
			inside := m[4]
			if i := strings.LastIndex(inside, " ["); i >= 0 && strings.HasSuffix(inside, "]") {
				inside = inside[:i] // the architecture
			}
			ver, rest, _ := strings.Cut(inside, " ")
			c.Version = ver
			for _, o := range strings.Split(rest, ",") {
				if o = strings.TrimSpace(o); o != "" && !slices.Contains(c.Origins, o) {
					c.Origins = append(c.Origins, o)
				}
			}
		}
		for _, v := range []string{c.From, c.Version} {
			if v != "" && !aptVersionRe.MatchString(v) {
				return nil, fmt.Errorf("apt-get -s line with a version mamelet does not read: %q", bounded(line, 200))
			}
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// newestRelease is when the newest release file of apt's lists was last modified (zero: none).
func newestRelease(r *os.Root) time.Time {
	var newest time.Time
	entries, err := fs.ReadDir(r.FS(), rel(aptListsDir))
	if err != nil {
		return newest
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, "_InRelease") && !strings.HasSuffix(n, "_Release") {
			continue
		}
		fi, err := r.Lstat(rel(aptListsDir + "/" + n))
		if err == nil && fi.Mode().IsRegular() && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if newest.IsZero() {
		return newest
	}
	return newest.UTC().Truncate(time.Second)
}

// dpkgInterrupted reports whether dpkg's journal has entries (/var/lib/dpkg/updates, files named by
// digits only): dpkg was cut short, and apt refuses to install until dpkg --configure -a (as apt
// itself checks).
func dpkgInterrupted(r *os.Root) bool {
	entries, err := fs.ReadDir(r.FS(), "var/lib/dpkg/updates")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if n := e.Name(); n != "" && strings.Trim(n, "0123456789") == "" {
			return true
		}
	}
	return false
}

// maxRebootPackages bounds the packages reported as needing a reboot.
const maxRebootPackages = 16

// rebootRequired reports whether the node says it needs a reboot (/run/reboot-required) and for which
// packages (/run/reboot-required.pkgs: package names only, each once, sorted, bounded).
func rebootRequired(r *os.Root) (bool, []string) {
	fi, err := r.Lstat("run/reboot-required")
	if err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	var pkgs []string
	if b, err := readRegular(r, "/run/reboot-required.pkgs", 16<<10); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); aptQualifiedRe.MatchString(l) && !slices.Contains(pkgs, l) {
				pkgs = append(pkgs, l)
			}
		}
	}
	slices.Sort(pkgs)
	if len(pkgs) > maxRebootPackages {
		pkgs = pkgs[:maxRebootPackages]
	}
	return true, pkgs
}

// actAptUpgrades carries out the approved upgrade, once per attempt, from a revision fetched this
// round; with no upgrade to carry out, it reads the package lists again if they are old and the part
// lets it. It never reads them while an upgrade waits: the approved versions may not be in newer lists.
func actAptUpgrades(ctx context.Context, x *mamelet.Act, p Part) bool {
	u := p.AptUpgrades
	if u == nil || !x.Online || ctx.Err() != nil {
		return false
	}
	if up := u.Upgrade; up != nil && !x.Done(up.Grant.Operation, up.Grant.Attempt) {
		outcome, detail, ran := upgradePackages(ctx, x.System, *up)
		x.Record(up.Grant.Operation, up.Grant.Attempt, outcome, detail)
		return ran
	}
	if u.Refresh {
		return refreshLists(ctx, x.System)
	}
	return false
}

// upgradePackages carries out one approved upgrade: it simulates the approved packages and versions
// again, and upgrades them only if the change set is still the approved one and the node's mamelet
// allows it (no removal, every origin one its configuration allows). An attempt that was cut short, or
// a person who upgraded meanwhile, is found by the simulation: nothing left to do is done; a part done
// is refused, and the next plan shows what is left. ran says whether it ran apt-get install.
func upgradePackages(ctx context.Context, s *mamelet.System, up AptUpgrade) (outcome, detail string, ran bool) {
	refused := func(why string) (string, string, bool) { return noderev.OutcomeRefused, why, false }
	if err := checkAptUpgrade(up); err != nil {
		return refused(err.Error())
	}
	fresh, err := simulate(ctx, s, "install", slices.Concat([]string{"--only-upgrade"}, up.Packages)...)
	if err != nil {
		return refused("the approved upgrade cannot be simulated again: " + bounded(err.Error(), maxAptDetail) + "; nothing was upgraded (plan again)")
	}
	changes, digest := aptChangeSet(fresh)
	switch {
	case len(changes) == 0:
		return noderev.OutcomeDone, fmt.Sprintf("the %d packages are at the approved versions already (an attempt that was cut short, or an upgrade by hand)", len(up.Packages)), false
	case digest != up.ChangeSet:
		return refused(fmt.Sprintf("upgrading would now change %s, not the approved %s (the package lists or the installed packages changed since the plan); nothing was upgraded (plan again)",
			digest, up.ChangeSet))
	}
	cs := noderev.ChangeSet{}
	for _, c := range changes {
		cs.Changes = append(cs.Changes, noderev.Change{Action: c.Action, Package: c.Package, Version: c.Version, Origins: c.Origins})
	}
	if why := s.Refusal(cs); why != "" {
		return refused(why + "; nothing was upgraded")
	}
	// Not cut short when mamelet is asked to stop: dpkg killed in the middle leaves the node for a
	// person to repair. It ends by itself, or at AptUpgradeTimeout.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), AptUpgradeTimeout)
	defer cancel()
	_, err = s.Run(uctx, progAptGet, slices.Concat(aptUpgradeArgs, []string{"install", "--only-upgrade"}, up.Packages), "DEBIAN_FRONTEND=noninteractive")
	if err != nil {
		return noderev.OutcomeFailed, "apt-get install --only-upgrade failed: " + bounded(err.Error(), maxAptDetail) +
			"; what it upgraded stays (the next plan shows what is left)", true
	}
	return noderev.OutcomeDone, fmt.Sprintf("upgraded %d packages (change set %s)", len(up.Packages), up.ChangeSet), true
}

// refreshLists runs apt-get update when the package lists are older than AptListsMaxAge (and an earlier
// attempt did not fail within AptRefreshRetry), and records how it went. It says whether it ran apt.
func refreshLists(ctx context.Context, s *mamelet.System) bool {
	dir := stateDir(s)
	if dir == "" {
		return false // no record: it could not tell when it last tried
	}
	r, err := openTree(s)
	if err != nil {
		return false
	}
	listsAt := newestRelease(r)
	r.Close()
	rec := loadRefresh(dir)
	now := aptNow().UTC().Truncate(time.Second)
	at := listsAt
	if rec.Succeeded.After(at) {
		at = rec.Succeeded
	}
	switch {
	case !at.IsZero() && now.Sub(at) <= AptListsMaxAge:
		return false
	case rec.FailedAt.After(rec.Succeeded) && now.Sub(rec.FailedAt) < AptRefreshRetry:
		return false
	}
	uctx, cancel := context.WithTimeout(ctx, AptUpdateTimeout)
	defer cancel()
	said, err := runSaid(uctx, s, progAptGet, []string{"-o", "DPkg::Lock::Timeout=300", "update"}, "DEBIAN_FRONTEND=noninteractive")
	switch {
	case ctx.Err() != nil:
		return true // cut short: the next round tries again
	case err != nil && strings.Contains(err.Error(), aptListsLock):
		return false // another apt is reading them now (apt-daily, or by hand)
	}
	failures := updateFailures(said)
	switch {
	case err != nil && uctx.Err() != nil:
		rec.Failed, rec.FailedAt = fmt.Sprintf("apt-get update did not finish within %s", AptUpdateTimeout), now
	case err != nil:
		rec.Failed, rec.FailedAt = bounded(err.Error(), maxAptDetail), now
		if failures != "" {
			rec.Failed = bounded(failures, maxAptDetail)
		}
	case failures != "":
		rec.Failed, rec.FailedAt = failures, now
	default:
		rec = refreshRecord{Succeeded: now}
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = saveJSON(dir, refreshName, rec)
	}
	return true
}

// updateFailures are the lines of what apt-get update said that say it did not read a source (Err:
// with the lines indented below it, E:, W: Failed to fetch, W: GPG error), joined with " | ",
// bounded ("" none).
func updateFailures(said string) string {
	var out []string
	inErr := false
	for _, line := range strings.Split(said, "\n") {
		if inErr && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			out = append(out, strings.TrimSpace(line))
			continue
		}
		inErr = false
		switch {
		case strings.HasPrefix(line, "Err:"):
			inErr = true
			out = append(out, line)
		case strings.HasPrefix(line, "E: "), strings.HasPrefix(line, "W: Failed to fetch"), strings.HasPrefix(line, "W: GPG error"):
			out = append(out, line)
		}
	}
	return bounded(strings.Join(out, " | "), maxAptDetail)
}

// noteUpgradeFailures adds to the observation why the last attempt of the upgrade the revision grants
// did not succeed (AptUpgradesState.Failed), from the recorded outcomes of the revision's grants.
func noteUpgradeFailures(p Part, results []noderev.Result, o *Observed) {
	if p.AptUpgrades == nil || p.AptUpgrades.Upgrade == nil || o.AptUpgrades == nil {
		return
	}
	g := p.AptUpgrades.Upgrade.Grant
	i := slices.IndexFunc(results, func(res noderev.Result) bool { return res.Operation == g.Operation && res.Attempt == g.Attempt })
	if i >= 0 && results[i].Outcome != noderev.OutcomeDone {
		o.AptUpgrades.Failed = bounded(results[i].Outcome+": "+results[i].Detail, maxAptFailed)
	}
}

// aptUpgradesResults are the recorded outcomes of the part's upgrade.
func aptUpgradesResults(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	if p.AptUpgrades == nil || p.AptUpgrades.Upgrade == nil {
		return nil
	}
	g := p.AptUpgrades.Upgrade.Grant
	if res, ok := known(g.Operation, g.Attempt); ok {
		return []noderev.Result{res}
	}
	return nil
}
