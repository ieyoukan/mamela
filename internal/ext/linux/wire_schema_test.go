package linux

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// schemaOf describes the JSON of a Go type as encoding/json/v2 writes it: each member's name and
// options, and its type, down to strings and numbers.
func schemaOf(t reflect.Type, indent string, b *strings.Builder) {
	switch t.Kind() {
	case reflect.Pointer:
		b.WriteString("*")
		schemaOf(t.Elem(), indent, b)
	case reflect.Slice:
		b.WriteString("[]")
		schemaOf(t.Elem(), indent, b)
	case reflect.Map:
		b.WriteString("map[" + t.Key().String() + "]")
		schemaOf(t.Elem(), indent, b)
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() {
			b.WriteString("time.Time")
			return
		}
		b.WriteString(t.String() + " {\n")
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" || !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" && !strings.Contains(opts, "inline") {
				name = f.Name
			}
			b.WriteString(indent + "  " + strconv.Quote(name))
			if opts != "" {
				b.WriteString(" (" + opts + ")")
			}
			b.WriteString(" ")
			schemaOf(f.Type, indent+"  ", b)
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	default:
		b.WriteString(t.Kind().String())
	}
}

// The JSON of the linux part of a revision and of a report is fixed for each Version: a released
// mamelet reads the part rejecting members it does not know, and a released Control Plane reads its
// report so (Observed). A member added, removed or changed for a released Version would make one of
// them refuse the other's: it needs a new Version (and Release), which the Control Plane gives only to
// mamelets that list it. While a Version is not released (0.10.0 for Version 1), its file is rewritten
// with -update; once it is, the file for it must not change.
func TestTheWireOfAVersionIsFixed(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "linux wire, Version %d (mamelet %s)\n\npart: ", Version, Release)
	schemaOf(reflect.TypeFor[Part](), "", &b)
	b.WriteString("\n\nreport: ")
	schemaOf(reflect.TypeFor[Observed](), "", &b)
	b.WriteString("\n")
	path := filepath.Join("testdata", "wire", fmt.Sprintf("v%d.txt", Version))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: a new Version has a file of its own (write it with -update)", err)
	}
	if string(want) != b.String() {
		t.Errorf("the JSON of the linux part or report of Version %d changed (%s):\n got: %s\nwant: %s\n"+
			"a mamelet of Release %s reads the part, and a Control Plane its report, rejecting members they do not know: "+
			"if %s is released, raise Version and Release (wire.go) instead; if it is not, rewrite the file with -update",
			Version, path, b.String(), want, Release, Release)
	}
}

// Every member of Part is one the core's handling of the part covers: an empty one leaves the part out
// of the revision (setPart), and a grant in it is found (partGrants: Sign refuses a revision whose
// grants it cannot read or that still have deadlines), handed out without its deadline or not at all
// once it ended (partForMamelet), taken away on a protected node (Protect), and checked by mamelet
// (checkPart). A kind that adds a member adds a sample of it here; a member without one fails.
func TestEveryMemberOfThePartIsCovered(t *testing.T) {
	once := func(action string, exp time.Time) noderev.Once {
		return noderev.Once{Operation: opA, Attempt: 1, Action: action, Expires: exp}
	}
	keys := KeysContent([]string{keyAlice})
	samples := map[string]func(exp time.Time) Part{
		"Files": func(exp time.Time) Part {
			f := grant("/etc/a.conf", "a\n", opA, 1)
			f.Write.Grant.Expires = exp
			return Part{Files: []File{f}}
		},
		"Users": func(exp time.Time) Part {
			return Part{Users: []User{{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{
				Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)}, Before: UserBefore{Exists: true, Keys: &Before{}},
				Grant: once(ActionChangeUser, exp)}}}}
		},
		"Artifacts": func(exp time.Time) Part {
			a := install("/usr/local/bin/a", "https://example.com/a", sumA, "", opA, 1)
			a.Install.Grant.Expires = exp
			return Part{Artifacts: []Artifact{a}}
		},
		"AptRepos": func(exp time.Time) Part {
			s := AptRepoSpec{Name: "docker", URIs: []string{"https://download.docker.com/linux/ubuntu"}, Suites: []string{"noble"},
				Components: []string{"stable"}, KeyURL: "https://download.docker.com/linux/ubuntu/gpg", KeySHA256: sumA}
			armored, _ := aptSourcesSHA256(s)
			return Part{AptRepos: []AptRepo{{Name: "docker", Where: []AptWhere{{URIs: s.URIs, Suites: s.Suites}}, Add: &AptRepoAdd{
				URIs: s.URIs, Suites: s.Suites, Components: s.Components, KeyURL: s.KeyURL, KeySHA256: s.KeySHA256, SourcesSHA256: armored,
				Grant: once(ActionAddAptRepo, exp)}}}}
		},
		"AptUpgrades": func(exp time.Time) Part {
			origins := []string{"Debian-Security:"}
			return Part{AptUpgrades: &AptUpgrades{Sets: [][]string{origins}, Refresh: true, Upgrade: &AptUpgrade{Origins: origins,
				Packages: []string{"bind9-libs=1:9.20.15-1~deb13u1"}, ChangeSet: "sha256:" + sumA, Grant: once(ActionUpgradePackages, exp)}}}
		},
	}
	if s := encode(Part{}); string(s) != "{}" {
		t.Errorf("an empty part is %s", s)
	}
	pt := reflect.TypeFor[Part]()
	for i := range pt.NumField() {
		field := pt.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			if tag := field.Tag.Get("json"); !strings.Contains(tag, "omitempty") && !strings.Contains(tag, "omitzero") {
				t.Errorf("tag %q: an empty member must be left out (setPart takes an empty part out by its JSON)", tag)
			}
			sample, ok := samples[field.Name]
			if !ok {
				t.Fatalf("Part.%s has no sample here: add one, with a grant", field.Name)
			}
			live := sample(mt0.Add(time.Hour))
			if reflect.ValueOf(live).Field(i).IsZero() {
				t.Fatalf("the sample does not set Part.%s", field.Name)
			}
			r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour)}
			setPart(&r, live)
			if _, ok := r.Ext[Name]; !ok {
				t.Fatal("setPart left a part with only this member out")
			}
			if err := checkPart(encode(live)); err != nil {
				t.Errorf("mamelet refuses the sample: %v", err)
			}
			gs, err := partGrants(r.Ext[Name])
			if err != nil || len(gs) != 1 || !gs[0].Expires.Equal(mt0.Add(time.Hour)) {
				t.Fatalf("partGrants: %+v %v", gs, err)
			}
			key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			if _, err := noderev.Sign(key, r.ForMamelet(mt0)); err != nil {
				t.Errorf("the revision as mamelet gets it is not signed: %v", err)
			}
			out, err := partForMamelet(r.Ext[Name], mt0)
			if gs, _ := partGrants(out); err != nil || len(gs) != 1 || !gs[0].Expires.IsZero() {
				t.Errorf("a live grant handed out: %+v %v", gs, err)
			}
			setPart(&r, sample(mt0.Add(-time.Minute)))
			out, err = partForMamelet(r.Ext[Name], mt0)
			if gs, _ := partGrants(out); err != nil || len(gs) != 0 {
				t.Errorf("an ended grant handed out: %+v %v", gs, err)
			}
			setPart(&r, live)
			for _, k := range kinds {
				p := partOf(&r)
				k.protect(&p)
				setPart(&r, p)
			}
			if gs, _ := partGrants(r.Ext[Name]); len(gs) != 0 {
				t.Errorf("a protected node keeps %+v", gs)
			}
		})
	}
}
