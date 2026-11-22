package linux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
)

func upgradesRes(spec string) string {
	return `{"kind":"NodeAptUpgrades","name":"web/apt-upgrades","spec":` + spec + `}`
}

var securityUpgrades = upgradesRes(`{"node":"web","origins":["Debian-Security:"]}`)

// The origins are a set: their order is not the graph's. The kind sorts after NodeFile.
func TestAptUpgradesCanonicalForm(t *testing.T) {
	a, err := decodeGraph(upgradesRes(`{"node":"web","origins":["Debian-Security:","Debian:"]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := decodeGraph(upgradesRes(`{"node":"web","origins":["Debian:","Debian-Security:"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Errorf("the order of the origins makes another graph:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	want := `{"kind":"NodeAptUpgrades","name":"web/apt-upgrades","spec":{"node":"web","origins":["Debian-Security:","Debian:"]}}`
	if !strings.Contains(string(a.Canonical()), want) {
		t.Errorf("canonical %s", a.Canonical())
	}
	g, err := decodeGraph(securityUpgrades, fileRes("web/file/etc/motd", `{"node":"web","path":"/etc/motd","content":"hi\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := g.Sorted(); s[1].Kind != KindFile || s[2].Kind != KindAptUpgrades {
		t.Errorf("order %v", s)
	}
	if d, _ := graph.KindOf(KindAptUpgrades); d.Order != 10 {
		t.Errorf("NodeAptUpgrades is at %d", d.Order)
	}
}

func TestAptUpgradesRejections(t *testing.T) {
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"no origins member", upgradesRes(`{"node":"web"}`), graph.CodeMissingField, "origins"},
		{"no origin", upgradesRes(`{"node":"web","origins":[]}`), graph.CodeInvalidValue, "there is no origin"},
		{"empty origin", upgradesRes(`{"node":"web","origins":[""]}`), graph.CodeInvalidValue, "not an APT origin"},
		// A beginning shorter than the label and its ":" would take any label that starts so.
		{"no label", upgradesRes(`{"node":"web","origins":["Debian"]}`), graph.CodeInvalidValue, "not an APT origin"},
		{"just a colon", upgradesRes(`{"node":"web","origins":[":13/stable"]}`), graph.CodeInvalidValue, "not an APT origin"},
		{"newline", upgradesRes(`{"node":"web","origins":["Debian:\nx"]}`), graph.CodeInvalidValue, "not an APT origin"},
		{"space after the colon", upgradesRes(`{"node":"web","origins":["Debian: 13"]}`), graph.CodeInvalidValue, "not an APT origin"},
		{"twice", upgradesRes(`{"node":"web","origins":["Debian:","Debian:"]}`), graph.CodeInvalidValue, "named twice"},
		{"nine", upgradesRes(`{"node":"web","origins":["a:","b:","c:","d:","e:","f:","g:","h:","i:"]}`), graph.CodeInvalidValue, "at most 8"},
		{"unknown member", upgradesRes(`{"node":"web","origins":["Debian:"],"all":true}`), graph.CodeUnknownField, "all"},
		{"name", `{"kind":"NodeAptUpgrades","name":"web/upgrades","spec":{"node":"web","origins":["Debian:"]}}`, graph.CodeNameMismatch, `"web/apt-upgrades"`},
		{"node", `{"kind":"NodeAptUpgrades","name":"db/apt-upgrades","spec":{"node":"db","origins":["Debian:"]}}`, graph.CodeDanglingNode, "db"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeGraph(c.res)
			var ge *graph.Error
			if !errors.As(err, &ge) {
				t.Fatalf("accepted (%v)", err)
			}
			for _, p := range ge.Problems {
				if p.Code == c.code && strings.Contains(p.Path+" "+p.Message+" "+p.Hint, c.says) {
					return
				}
			}
			t.Errorf("no %s saying %q in %v", c.code, c.says, err)
		})
	}
	// One a node: the name is the node's.
	if _, err := decodeGraph(securityUpgrades, upgradesRes(`{"node":"web","origins":["Debian:"]}`)); err == nil {
		t.Error("two upgrades of one node")
	}
	for _, ok := range []string{"Debian-Security:", "Debian-Security:13/stable-security", "Debian:13.6/stable", "Ubuntu:24.04/noble-security", "Docker CE:noble"} {
		if !ValidAptOrigin(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}

// What Mamela's Pome writes (pome/mamela/test/graph_test.emel) is the canonical form, byte for byte.
func TestAptUpgradesFromThePome(t *testing.T) {
	pome := `{"kind":"NodeAptUpgrades","name":"web/apt-upgrades","spec":{"node":"web","origins":["Debian-Security:","Debian:"]}}`
	g, err := decodeGraph(pome)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(g.Canonical()), pome) {
		t.Errorf("%s is not canonical: %s", pome, g.Canonical())
	}
}

// apt-get -s dist-upgrade on a Debian 13 machine some security updates and a point release behind
// (apt 3.0, as root: no simulation note). Many security updates are in the main archive too after a
// point release: the candidate has both origins.
const trixieDistUpgrade = `Reading package lists...
Building dependency tree...
Reading state information...
Calculating upgrade...
The following packages will be upgraded:
  base-files bind9-dnsutils bind9-host bind9-libs libc6:i386 libssl3t64 openssl
  openssl-provider-legacy
8 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
Inst base-files [13.8+deb13u6] (13.8+deb13u8 Debian:13.6/stable [amd64])
Conf base-files (13.8+deb13u8 Debian:13.6/stable [amd64])
Inst bind9-dnsutils [1:9.20.23-1~deb13u1] (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64]) []
Inst bind9-host [1:9.20.23-1~deb13u1] (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64]) []
Inst bind9-libs [1:9.20.23-1~deb13u1] (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64])
Inst libc6:i386 [2.41-12] (2.41-12+deb13u1 Debian:13.6/stable [i386])
Inst openssl-provider-legacy [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64]) [openssl:amd64 libssl3t64:amd64 ]
Inst openssl [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64]) [libssl3t64:amd64 ]
Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
Conf bind9-libs (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64])
Conf bind9-host (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64])
Conf bind9-dnsutils (1:9.20.30-1~deb13u1 Debian:13.6/stable, Debian-Security:13/stable-security [amd64])
Conf libc6:i386 (2.41-12+deb13u1 Debian:13.6/stable [i386])
Conf libssl3t64 (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
Conf openssl (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
Conf openssl-provider-legacy (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
`

func TestParseAptSimulation(t *testing.T) {
	got, err := parseAptSimulation(trixieDistUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("%d changes: %+v", len(got), got)
	}
	want := AptChange{Action: noderev.ChangeUpgrade, Package: "bind9-dnsutils", From: "1:9.20.23-1~deb13u1", Version: "1:9.20.30-1~deb13u1",
		Origins: []string{"Debian:13.6/stable", "Debian-Security:13/stable-security"}}
	if c := got[1]; c.Action != want.Action || c.Package != want.Package || c.From != want.From || c.Version != want.Version || !slices.Equal(c.Origins, want.Origins) {
		t.Errorf("bind9-dnsutils: %+v", c)
	}
	if c := got[4]; c.Package != "libc6:i386" || c.Version != "2.41-12+deb13u1" || !slices.Equal(c.Origins, []string{"Debian:13.6/stable"}) {
		t.Errorf("a foreign architecture's package: %+v", c)
	}
	if c := got[5]; c.Package != "openssl-provider-legacy" || c.Version != "3.5.1-1+deb13u1" {
		t.Errorf("a line with the packages it breaks: %+v", c)
	}

	// The task's own example, with a new dependency and a removal.
	one, err := parseAptSimulation(`NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
Inst bind9-libs [1:9.20.11-4] (1:9.20.15-1~deb13u1 Debian-Security:13/stable-security [amd64])
Inst linux-image-6.12.57+deb13-amd64 (6.12.57-1 Debian-Security:13/stable-security [amd64])
Remv libfoo1 [1.0-1]
Purg libbar2 [2.0-1] [libbaz:amd64 ]
Conf bind9-libs (1:9.20.15-1~deb13u1 Debian-Security:13/stable-security [amd64])
`)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, c := range one {
		lines = append(lines, c.Line())
	}
	if got := strings.Join(lines, "\n"); got != `upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security)
install linux-image-6.12.57+deb13-amd64 6.12.57-1 (Debian-Security:13/stable-security)
remove libfoo1 1.0-1
remove libbar2 2.0-1` {
		t.Errorf("lines:\n%s", got)
	}

	for _, bad := range []string{
		"Inst bind9-libs [1:9.20.11-4]",                       // no candidate
		"Inst Bind9 [1] (2 Debian:13/stable [amd64])",         // not a package name
		"Inst bind9-libs [1] (2;rm Debian:13/stable [amd64])", // not a version
		"Inst bind9-libs [1] (2 (Debian:13/stable [amd64])",   // a parenthesis inside
		"Remv libfoo1 [1.0-1] (2.0 Debian:13/stable [amd64])", // a removal that installs
	} {
		if _, err := parseAptSimulation(bad + "\n"); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}

// The packages to upgrade are those whose candidate comes from one of the origins, pinned to it: the
// security archive's alone, or with the point release's too.
func TestUpgradesFromTheDeclaredOrigins(t *testing.T) {
	sim, err := parseAptSimulation(trixieDistUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	security := upgradesFrom(sim, []string{"Debian-Security:"})
	if want := []string{"bind9-dnsutils=1:9.20.30-1~deb13u1", "bind9-host=1:9.20.30-1~deb13u1", "bind9-libs=1:9.20.30-1~deb13u1",
		"libssl3t64=3.5.1-1+deb13u1", "openssl-provider-legacy=3.5.1-1+deb13u1", "openssl=3.5.1-1+deb13u1"}; !slices.Equal(security, want) {
		t.Errorf("security:\n got %v\nwant %v", security, want)
	}
	both := upgradesFrom(sim, []string{"Debian-Security:", "Debian:"})
	if len(both) != 8 || !slices.Contains(both, "base-files=13.8+deb13u8") || !slices.Contains(both, "libc6:i386=2.41-12+deb13u1") {
		t.Errorf("security and point release: %v", both)
	}
	// "Debian:" is not a beginning of "Debian-Security:...".
	if point := upgradesFrom(sim, []string{"Debian:"}); len(point) != 5 || slices.Contains(point, "openssl=3.5.1-1+deb13u1") {
		t.Errorf("point release: %v", point)
	}
	if none := upgradesFrom(sim, []string{"Ubuntu:"}); len(none) != 0 {
		t.Errorf("another distribution's: %v", none)
	}
	for _, pin := range both {
		if _, _, ok := splitPin(pin); !ok {
			t.Errorf("%q is not a pin mamelet passes", pin)
		}
	}
}

// The change set's digest is of its lines whatever their order, and changes with any of them.
func TestAptChangeSetDigest(t *testing.T) {
	a := []AptChange{{Action: "upgrade", Package: "b", From: "1", Version: "2", Origins: []string{"Y:1/s", "X:1/s"}}, {Action: "install", Package: "a", Version: "1"}}
	b := []AptChange{{Action: "install", Package: "a", Version: "1"}, {Action: "upgrade", Package: "b", From: "1", Version: "2", Origins: []string{"X:1/s", "Y:1/s"}}}
	sa, da := aptChangeSet(a)
	_, db := aptChangeSet(b)
	if da != db || sa[0].Package != "a" || sa[1].Origins[0] != "X:1/s" {
		t.Errorf("%s %s %+v", da, db, sa)
	}
	c := slices.Clone(b)
	c[1].From = "0"
	if _, dc := aptChangeSet(c); dc == da {
		t.Error("another installed version, the same digest")
	}
	if _, empty := aptChangeSet(nil); !strings.HasPrefix(empty, "sha256:") {
		t.Errorf("empty %s", empty)
	}
}

// What mamelet refuses of an approved upgrade in its revision.
func TestAptUpgradeChecks(t *testing.T) {
	good := AptUpgrade{Origins: []string{"Debian-Security:"}, Packages: []string{"bind9-libs=1:9.20.15-1~deb13u1"}, ChangeSet: "sha256:" + sumA,
		Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionUpgradePackages}}
	if err := checkAptUpgrade(good); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(u *AptUpgrade){
		"another action":   func(u *AptUpgrade) { u.Grant.Action = ActionWrite },
		"no packages":      func(u *AptUpgrade) { u.Packages = nil },
		"no version":       func(u *AptUpgrade) { u.Packages = []string{"bind9-libs"} },
		"an option":        func(u *AptUpgrade) { u.Packages = []string{"-oDebug::X=1=1"} },
		"a space":          func(u *AptUpgrade) { u.Packages = []string{"bind9-libs=1 2"} },
		"twice":            func(u *AptUpgrade) { u.Packages = []string{"a1=1", "a1=2"} },
		"no change set":    func(u *AptUpgrade) { u.ChangeSet = sumA },
		"no origin":        func(u *AptUpgrade) { u.Origins = nil },
		"an origin refuse": func(u *AptUpgrade) { u.Origins = []string{"D"} },
	} {
		u := good
		u.Packages = slices.Clone(good.Packages)
		f(&u)
		if err := checkAptUpgrade(u); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
