package linux

// The golden plans of NodeAptUpgrades, as those of NodeFile (golden_test.go): each scenario's
// declaration, observation and policy, and the plan made of them, under
// testdata/golden/plan/upgrades-<scenario>. Regenerate them only for a change that means to change
// them:
//
//	go test ./internal/ext/linux -run Golden -update

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/site"
)

// pendingSet is what mamelet reports for Debian's security archive on a Debian 13 machine: bind9's
// libraries and tools, whose candidate is in the security archive (and, after a point release, in the
// main archive too), plus what more lines say.
func pendingSet(more ...AptChange) AptUpgradeSet {
	sec := []string{"Debian-Security:13/stable-security"}
	changes := append([]AptChange{
		{Action: noderev.ChangeUpgrade, Package: "bind9-libs", From: "1:9.20.11-4", Version: "1:9.20.15-1~deb13u1", Origins: sec},
		{Action: noderev.ChangeUpgrade, Package: "bind9-dnsutils", From: "1:9.20.11-4", Version: "1:9.20.15-1~deb13u1", Origins: []string{"Debian-Security:13/stable-security", "Debian:13.6/stable"}},
		{Action: noderev.ChangeUpgrade, Package: "libssl3t64", From: "3.5.1-1", Version: "3.5.1-1+deb13u1", Origins: sec},
	}, more...)
	sorted, digest := aptChangeSet(changes)
	return AptUpgradeSet{Origins: []string{"Debian-Security:"}, Packages: []string{"bind9-dnsutils=1:9.20.15-1~deb13u1",
		"bind9-libs=1:9.20.15-1~deb13u1", "libssl3t64=3.5.1-1+deb13u1"}, Changes: sorted, Digest: digest}
}

func TestGoldenAptUpgradesPlans(t *testing.T) {
	now := world{mamelet: Release + "+abc", accepts: Version}
	asOf := gt0.Add(time.Minute) // the snapshot's
	reported := func(st AptUpgradesState) world {
		w := now
		w.observed = &Observed{AptUpgrades: &st}
		return w
	}
	fresh := func(sets ...AptUpgradeSet) AptUpgradesState {
		return AptUpgradesState{ListsAt: gt0.Add(-3 * time.Hour), RefreshedAt: gt0.Add(-40 * time.Minute), Sets: sets}
	}
	debian := policy.Default
	debian.AllowedAptOrigins = []string{"Debian-Security:", "Debian:"}
	securityOnly := policy.Default
	securityOnly.AllowedAptOrigins = []string{"Debian-Security:"}
	narrow := policy.Default
	narrow.AllowedAptOrigins = []string{"Debian-Security:13/", "Debian:"}
	observeOnly := debian
	observeOnly.ObserveOnly = []string{"web"}

	inSync := AptUpgradeSet{Origins: []string{"Debian-Security:"}}
	removes := pendingSet(AptChange{Action: noderev.ChangeRemove, Package: "libisc-export1105", From: "1:9.11.19+dfsg-2.1"})
	newDep := pendingSet(AptChange{Action: noderev.ChangeInstall, Package: "libuv1t64", Version: "1.50.0-2", Origins: []string{"Debian:13.6/stable"}})
	year := AptUpgradesState{ListsAt: gt0.Add(-400 * 24 * time.Hour), Sets: []AptUpgradeSet{inSync}}
	failing := year
	failing.RefreshFailedAt, failing.RefreshFailed = gt0.Add(-20*time.Minute),
		"W: Failed to fetch http://security.debian.org/debian-security/dists/trixie-security/InRelease  Temporary failure resolving 'security.debian.org' | "+
			"W: Failed to fetch http://deb.debian.org/debian/dists/trixie/InRelease  Temporary failure resolving 'deb.debian.org'"
	reboot := fresh(inSync)
	reboot.RebootRequired, reboot.RebootPackages = true, []string{"linux-image-6.12.57+deb13-amd64"}
	partial := fresh(pendingSet())
	partial.Failed = "failed: apt-get install --only-upgrade failed: /usr/bin/apt-get ...: exit 100: E: Sub-process /usr/bin/dpkg returned an error code (1); what it upgraded stays (the next plan shows what is left)"
	dpkg := fresh(pendingSet())
	dpkg.DpkgInterrupted = true
	partly := fresh(pendingSet())
	partly.RefreshFailedAt, partly.RefreshFailed = gt0.Add(-10*time.Minute), "W: Failed to fetch https://download.docker.com/linux/debian/dists/trixie/InRelease  Could not resolve 'download.docker.com'"
	both := pendingSet(AptChange{Action: noderev.ChangeUpgrade, Package: "base-files", From: "13.8+deb13u6", Version: "13.8+deb13u8", Origins: []string{"Debian:13.6/stable"}})
	both.Origins = []string{"Debian-Security:", "Debian:"}
	both.Packages = append([]string{"base-files=13.8+deb13u8"}, both.Packages...)

	for _, s := range []struct {
		name     string
		resource string
		world    world
		pol      policy.Policy
	}{
		{"upgrades-pending", securityUpgrades, reported(fresh(pendingSet())), debian},
		{"upgrades-in-sync", securityUpgrades, reported(fresh(inSync)), debian},
		{"upgrades-security-and-point-release", upgradesRes(`{"node":"web","origins":["Debian-Security:","Debian:"]}`), reported(fresh(both)), debian},
		{"upgrades-removal-denied", securityUpgrades, reported(fresh(removes)), debian},
		// A dependency from the main archive, which the policy does not allow.
		{"upgrades-origin-denied", securityUpgrades, reported(fresh(newDep)), securityOnly},
		// The declaration names more of an origin than the policy allows.
		{"upgrades-declared-origin-denied", securityUpgrades, reported(fresh(pendingSet())), narrow},
		{"upgrades-observe-only", securityUpgrades, reported(fresh(pendingSet())), observeOnly},
		{"upgrades-stale-refresh-failed", securityUpgrades, reported(failing), debian},
		{"upgrades-stale-observe-only", securityUpgrades, reported(year), observeOnly},
		{"upgrades-stale-waits", securityUpgrades, reported(year), debian},
		{"upgrades-no-lists", securityUpgrades, reported(AptUpgradesState{Sets: []AptUpgradeSet{inSync}}), debian},
		{"upgrades-some-source-failed", securityUpgrades, reported(partly), debian},
		{"upgrades-reboot-required", securityUpgrades, reported(reboot), debian},
		{"upgrades-after-partial", securityUpgrades, reported(partial), debian},
		{"upgrades-dpkg-interrupted", securityUpgrades, reported(dpkg), debian},
		{"upgrades-awaiting-report", securityUpgrades, reported(fresh()), debian},
		{"upgrades-simulation-failed", securityUpgrades, reported(fresh(AptUpgradeSet{Origins: []string{"Debian-Security:"},
			Error: "apt-get -s install --only-upgrade: /usr/bin/apt-get ...: exit 100: E: Unable to correct problems, you have held broken packages."})), debian},
		{"upgrades-old-mamelet", securityUpgrades, world{mamelet: "0.9.2"}, debian},
	} {
		t.Run(s.name, func(t *testing.T) {
			g, err := decodeGraph(s.resource)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
			if !snap.AsOf.Equal(asOf) {
				t.Fatalf("the snapshot is of %s", snap.AsOf)
			}
			p := plan.BuildFor(g, snap, s.pol, goldenProviders)
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
			if q := plan.BuildFor(g, snap, s.pol, goldenProviders); q.Digest != p.Digest {
				t.Error("the plan is not deterministic")
			}
			// An upgrade is never automatic: approved, or denied.
			for _, c := range p.Changes {
				if c.Kind == KindAptUpgrades && c.Status == plan.StatusChange && c.Verdict == policy.Auto {
					t.Errorf("an automatic upgrade: %+v", c)
				}
			}
		})
	}
}
