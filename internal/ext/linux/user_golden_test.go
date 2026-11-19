package linux

// The golden plans of NodeUser, as those of NodeFile (golden_test.go): testdata/golden/plan/user-*.
// Regenerate them only for a change that means to change what they fix:
//
//	go test ./internal/ext/linux -run Golden -update

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/state"
)

func TestGoldenUserPlans(t *testing.T) {
	keys := func(lines ...string) string {
		for i, l := range lines {
			lines[i] = jsonString(l)
		}
		return `[` + strings.Join(lines, ",") + `]`
	}
	app := userRes("web/user/app", `{"node":"web","name":"app","groups":["docker","www-data"],"authorizedKeys":`+keys(keyAlice, keyBob)+`}`)
	plain := userRes("web/user/app", `{"node":"web","name":"app"}`)
	docker := `{"kind":"NodePackage","name":"web/docker-ce","spec":{"node":"web","package":"docker-ce","state":"Present"}}`
	now := world{mamelet: Release + "+abc", accepts: Version}
	// observe is a report of users and of the groups the part names.
	observe := func(w world, groups map[string]bool, users ...UserState) world {
		names := map[string]bool{}
		for _, u := range users {
			names[u.Name] = u.Exists
		}
		w.observed = &Observed{UserStates: users, Users: names, Groups: groups}
		return w
	}
	bash := map[string]bool{DefaultShell: true}
	// noHome: nothing is at the home app would be made with.
	noHome := []HomeState{{Path: "/home/app"}}
	// existing is app as useradd made it (uid 1001, group app), in groups.
	existing := func(groups ...string) UserState {
		return UserState{Name: "app", Exists: true, UID: 1001, GID: 1001, Group: "app", Home: "/home/app", Shell: DefaultShell, Groups: groups, Shells: bash}
	}
	// keysAt is app's authorized_keys as Mamela writes it, with the keys found.
	keysAt := func(content string, found ...FoundKey) *KeysState {
		return &KeysState{File: FileState{Path: "/home/app/.ssh/authorized_keys", Exists: true, SHA256: ContentSHA256(content), Mode: "0600", Owner: "app", Group: "app"},
			Dir: "0700 app:app", Found: found}
	}
	alice := FoundKey{Type: "ssh-ed25519", Fingerprint: fpAlice}
	bob := FoundKey{Type: "ecdsa-sha2-nistp256", Fingerprint: fpBob}
	oldCI := FoundKey{Type: "ssh-rsa", Fingerprint: fpOldCI}
	inSync := func() UserState {
		u := existing("docker", "www-data")
		u.Keys = keysAt(keyAlice+"\n"+keyBob+"\n", alice, bob)
		return u
	}
	groupsOK := accountsOf("app", "docker", "www-data")
	pol := policy.Default

	for _, s := range []struct {
		name      string
		resources []string
		world     world
		packages  map[string]bool // NodePackage web/<name>: installed
	}{
		{"user-create", []string{app}, observe(now, accountsOf("!app", "docker", "www-data"), UserState{Name: "app", Shells: bash, NewHomes: noHome}), nil},
		// The declared home is another user's directory: useradd would take it as it is.
		{"user-create-home-taken", []string{userRes("web/user/app", `{"node":"web","name":"app","home":"/home/bob"}`)}, observe(now, accountsOf("!app"),
			UserState{Name: "app", Shells: bash, NewHomes: []HomeState{{Path: "/home/bob", Exists: true, What: "a directory", Mode: "0750", Owner: "bob", Group: "bob"}}}), nil},
		// The home would be made below a directory everyone may write: someone could put theirs there
		// before useradd makes it.
		{"user-create-home-exposed", []string{app}, observe(now, accountsOf("!app", "docker", "www-data"),
			UserState{Name: "app", Shells: bash, NewHomes: []HomeState{{Path: "/home/app", Exposed: []DirOwner{{Dir: "/home", Mode: "0777", Owner: "root", Group: "root"}}}}}), nil},
		// A system user with keys and no declared home: its home is named (/home/svc), shown and looked at.
		{"user-create-system-keys", []string{userRes("web/user/svc", `{"node":"web","name":"svc","system":true,"authorizedKeys":`+keys(keyAlice)+`}`)},
			observe(now, accountsOf("!svc"), UserState{Name: "svc", Shells: map[string]bool{DefaultSystemShell: true}, NewHomes: []HomeState{{Path: "/home/svc"}}}), nil},
		// The user's home (from /etc/passwd) is another's: its keys are not written there.
		{"user-keys-home-not-owned", []string{app}, observe(now, groupsOK, func() UserState {
			u := existing("docker", "www-data")
			u.Home = "/home/bob"
			u.Keys = &KeysState{File: FileState{Path: "/home/bob/.ssh/authorized_keys", Exists: true, SHA256: ContentSHA256("x"), Mode: "0600", Owner: "bob", Group: "bob"},
				Dir: "0700 bob:bob", NotOwned: "/home/bob is bob's (uid 1000), not app's", Found: []FoundKey{oldCI}}
			return u
		}()), nil},
		// A file of more keys than the agent lists: the rest are counted, and taken out.
		{"user-keys-more", []string{app}, observe(now, groupsOK, func() UserState {
			u := existing("docker", "www-data")
			u.Keys = keysAt("many\n", alice, oldCI)
			u.Keys.More = 70
			return u
		}()), nil},
		{"user-create-system", []string{userRes("web/user/svc", `{"node":"web","name":"svc","system":true,"uid":990,"groups":["adm"]}`)},
			observe(now, accountsOf("!svc", "adm"), UserState{Name: "svc", Shells: map[string]bool{DefaultSystemShell: true}, UIDs: map[string]string{"990": ""}}), nil},
		{"user-in-sync", []string{app}, observe(now, groupsOK, inSync()), nil},
		{"user-as-is", []string{userRes("web/user/app", `{"node":"web","name":"app","groups":["www-data"]}`)}, observe(now, accountsOf("app", "www-data"), func() UserState {
			u := existing("adm", "sudo", "www-data")
			u.UID, u.GID = 998, 998
			return u
		}()), nil},
		{"user-add-group", []string{app}, observe(now, groupsOK, func() UserState {
			u := inSync()
			u.Groups = []string{"adm", "www-data"}
			return u
		}()), nil},
		{"user-change-shell", []string{userRes("web/user/app", `{"node":"web","name":"app","shell":"/usr/bin/zsh"}`)}, observe(now, accountsOf("app"), func() UserState {
			u := existing()
			u.Shells = map[string]bool{"/usr/bin/zsh": true}
			return u
		}()), nil},
		{"user-keys-add-remove", []string{app}, observe(now, groupsOK, func() UserState {
			u := inSync()
			u.Keys = keysAt(keyBob+"\n"+keyOldCI+"\n", bob, oldCI)
			u.Keys.File.Mode, u.Keys.File.Owner, u.Keys.File.Group = "0644", "root", "root"
			u.Keys.Other = 1
			return u
		}()), nil},
		{"user-keys-first", []string{app}, observe(now, groupsOK, func() UserState {
			u := inSync()
			u.Keys = &KeysState{File: FileState{Path: "/home/app/.ssh/authorized_keys", Missing: []string{"/home/app/.ssh"}}}
			return u
		}()), nil},
		{"user-keys-symlink", []string{app}, observe(now, groupsOK, func() UserState {
			u := inSync()
			u.Keys = &KeysState{File: FileState{Path: "/home/app/.ssh/authorized_keys", Unsafe: "/home/app/.ssh is a symbolic link"}}
			return u
		}()), nil},
		{"user-uid-mismatch", []string{userRes("web/user/app", `{"node":"web","name":"app","uid":1500}`)}, observe(now, accountsOf("app"), existing()), nil},
		{"user-home-mismatch", []string{userRes("web/user/app", `{"node":"web","name":"app","home":"/srv/app"}`)}, observe(now, accountsOf("app"), existing()), nil},
		{"user-uid-0", []string{plain}, observe(now, accountsOf("app"), func() UserState {
			u := existing()
			u.UID = 0
			return u
		}()), nil},
		{"user-uid-taken", []string{userRes("web/user/app", `{"node":"web","name":"app","uid":1001}`)},
			observe(now, accountsOf("!app"), UserState{Name: "app", Shells: bash, UIDs: map[string]string{"1001": "alice"}}), nil},
		{"user-group-taken", []string{plain}, observe(now, accountsOf("app"), UserState{Name: "app", Shells: bash}), nil},
		{"user-group-waits-for-package", []string{app, docker}, observe(now, accountsOf("!app", "!docker", "www-data"), UserState{Name: "app", Shells: bash}), map[string]bool{"docker-ce": false}},
		{"user-group-missing", []string{app, docker}, observe(now, accountsOf("!app", "!docker", "www-data"), UserState{Name: "app", Shells: bash}), map[string]bool{"docker-ce": true}},
		{"user-shell-missing", []string{userRes("web/user/app", `{"node":"web","name":"app","shell":"/usr/bin/zsh"}`)},
			observe(now, accountsOf("app"), func() UserState {
				u := existing()
				u.Shells = map[string]bool{"/usr/bin/zsh": false}
				return u
			}()), nil},
		{"user-awaiting-report", []string{app}, observe(now, accountsOf("app", "www-data"), existing("www-data")), nil},
		{"user-file-waits-for-user", []string{plain, fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"app","group":"app"}`)},
			func() world {
				w := observe(now, accountsOf("root", "!app"), UserState{Name: "app", Shells: bash, NewHomes: noHome})
				w.observed.Files = []FileState{{Path: "/etc/app.conf"}}
				w.observed.Users["root"] = true
				return w
			}(), nil},
		{"user-old-mamelet", []string{app}, func() world { w := now; w.mamelet, w.accepts = "0.9.2", 0; return w }(), nil},
		{"user-kept", []string{}, func() world {
			w := observe(now, groupsOK, inSync(), UserState{Name: "gone"})
			w.observed.InForce = &InForce{Users: []string{"app", "gone"}} // the declaration in force has them
			return w
		}(), nil},
	} {
		t.Run(s.name, func(t *testing.T) {
			g, err := decodeGraph(s.resources...)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
			for name, installed := range s.packages {
				snap.Packages["web/"+name] = state.Package{Installed: installed, Version: "1", ObservedAt: gt0}
			}
			if snap, err = state.Decode(snap.Canonical()); err != nil {
				t.Fatal(err)
			}
			providers := goldenProviders
			p := plan.BuildFor(g, snap, pol, providers)
			dir := "plan/" + s.name + "/"
			golden(t, dir+"graph.json", append(g.Canonical(), '\n'))
			golden(t, dir+"snapshot.json", append(snap.Canonical(), '\n'))
			golden(t, dir+"policy.json", append(pol.Canonical(), '\n'))
			golden(t, dir+"plan.json", append(p.Canonical(), '\n'))
			golden(t, dir+"digest.txt", []byte("plan "+p.Digest+"\ngraph "+g.Digest()+"\nsnapshot "+snap.Digest()+"\n"))
			var r bytes.Buffer
			plan.Render(&r, p)
			golden(t, dir+"render.txt", r.Bytes())
			req, err := json.Marshal((&site.Site{}).PlannedRequirements(p, g, pol), json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			golden(t, dir+"requirements.json", append(req, '\n'))
			if q := plan.BuildFor(g, snap, pol, providers); q.Digest != p.Digest {
				t.Error("the plan is not deterministic")
			}
			for _, c := range p.Changes {
				if c.Kind == KindUser && c.Status == plan.StatusChange && (c.Verdict != policy.ApprovalRequired ||
					!slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == OpCreateUser || o.Op == OpChangeUser })) {
					t.Errorf("a user is changed without an approval: %+v", c)
				}
			}
		})
	}
}
