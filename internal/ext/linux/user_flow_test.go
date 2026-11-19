package linux

import (
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// userFlow is a flow (flow_test.go) whose node has root, the groups docker and www-data, the shells,
// and useradd and usermod (fakeAccounts).
func userFlow(t *testing.T, resources ...string) (*flow, *fakeAccounts) {
	f := newFlow(t, resources...)
	n, accounts := newUserNode(t)
	// The flow's node, with the user node's file system and programs.
	f.node.root, f.node.a.System = n.root, n.a.System
	accounts.root = n.root
	return f, accounts
}

func (f *flow) userChange() plan.Change {
	f.t.Helper()
	p, _ := f.plan()
	for _, c := range p.Changes {
		if c.Kind == KindUser {
			return c
		}
	}
	f.t.Fatalf("no user in the plan %+v", p.Changes)
	return plan.Change{}
}

// The whole way of a user: the plan asks to make it, the apply grants exactly what the plan showed,
// mamelet makes it; a later change of its keys says which go (and that logins with them stop); a
// file it owns waited for it; taken out of the declaration, it stays.
func TestAUserFromDeclarationToNode(t *testing.T) {
	declared := userRes("web/user/app", `{"node":"web","name":"app","groups":["docker"],"authorizedKeys":["`+keyAlice+`","`+keyBob+`"]}`)
	owned := fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"app","group":"app","mode":"0640"}`)
	f, accounts := userFlow(t, declared, owned)
	f.publish()
	f.node.round() // it says it takes linux
	if c := f.userChange(); c.Status != plan.StatusWaiting || c.Note.Code != codeUserAwait {
		t.Fatalf("before it observed the user: %+v", c)
	}
	f.node.round() // it observes the user
	p, _ := f.plan()
	var user, file plan.Change
	for _, c := range p.Changes {
		switch c.Kind {
		case KindUser:
			user = c
		case KindFile:
			file = c
		}
	}
	if user.Status != plan.StatusChange || user.Verdict != policy.ApprovalRequired || user.Operations[0].Op != OpCreateUser {
		t.Fatalf("the plan %+v", user)
	}
	if file.Status != plan.StatusWaiting || file.Note.Code != codeFileAccountComing {
		t.Fatalf("the file %+v", file)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	want := []string{"useradd --create-home --shell /bin/bash --home-dir /home/app --user-group -- app", "usermod --append --groups docker -- app"}
	if !slices.Equal(accounts.commands, want) {
		t.Fatalf("ran %q", accounts.commands)
	}
	if got := f.node.read("/home/app/.ssh/authorized_keys"); got != keyAlice+"\n"+keyBob+"\n" {
		t.Fatalf("authorized_keys %q", got)
	}
	f.node.round()
	if c := f.userChange(); c.Status != plan.StatusInSync {
		t.Fatalf("after the creation: %+v", c)
	}
	// The file owned by the user is written now.
	p, _ = f.plan()
	if i := slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Kind == KindFile }); p.Changes[i].Status != plan.StatusChange {
		t.Fatalf("the file %+v", p.Changes[i])
	}
	f.apply(p)
	f.node.round()

	// Bob's key goes, old-ci's comes: the plan says what goes, and warns.
	accounts.commands = nil
	f.declare(userRes("web/user/app", `{"node":"web","name":"app","groups":["docker"],"authorizedKeys":["`+keyAlice+`","`+keyOldCI+`"]}`), owned)
	f.publish()
	f.node.round()
	c := f.userChange()
	if c.Status != plan.StatusChange || c.Operations[0].Op != OpChangeUser || !slices.ContainsFunc(c.Diffs, func(d plan.Diff) bool { return d.Field == diffRemoveKey && strings.Contains(d.To, fpBob) }) ||
		!slices.ContainsFunc(c.Warnings, func(n plan.Note) bool { return n.Code == codeUserLockout }) {
		t.Fatalf("the change %+v", c)
	}
	p, _ = f.plan()
	f.apply(p)
	if got := f.node.read("/home/app/.ssh/authorized_keys"); got != keyAlice+"\n"+keyOldCI+"\n" || len(accounts.commands) != 0 {
		t.Fatalf("authorized_keys %q, ran %q", got, accounts.commands)
	}
	f.node.round()
	if c := f.userChange(); c.Status != plan.StatusInSync {
		t.Fatalf("after the change: %+v", c)
	}

	// Taken out of the declaration: the user stays (and its file), and mamelet stops watching it.
	f.declare()
	p, _ = f.plan()
	i := slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Kind == KindUser })
	if i < 0 || p.Changes[i].Status != plan.StatusRetained || p.Changes[i].Note.Code != codeUserKept {
		t.Fatalf("kept %+v", p.Changes)
	}
	f.apply(p)
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("still watched: %s", r.Content())
	}
	if !strings.Contains(f.node.read("/etc/passwd"), "\napp:") {
		t.Error("the user was removed")
	}
}

// An approval is for what the plan showed: a grant made from a plan whose keys are not the
// declaration's (it changed meanwhile) is refused before anything is granted.
func TestAUserGrantIsWhatThePlanShowed(t *testing.T) {
	s := &UserSpec{Node: "web", Name: "app", Groups: []string{"docker"}, AuthorizedKeys: []string{keyAlice}}
	sum := ContentSHA256(s.KeysContent())
	create := []plan.Diff{{Field: diffUser, To: "app"}, {Field: diffShell, To: DefaultShell}, {Field: diffHome, To: "/home/app (made)"},
		{Field: diffGroup, To: "app (the user's own, made)"}, {Field: diffGroups, To: "docker"}, {Field: diffKeysSHA, To: sum}}
	c, err := approvedUserChange(s, create, true)
	if err != nil || c.Create == nil || c.Create.Home != "/home/app" || !c.Create.CreateHome || !slices.Equal(c.Groups, []string{"docker"}) || c.Keys.SHA256 != sum {
		t.Fatalf("%+v %v", c, err)
	}
	for name, diffs := range map[string][]plan.Diff{
		"other keys":    replaced(create, plan.Diff{Field: diffKeysSHA, To: ContentSHA256("other")}),
		"other groups":  replaced(create, plan.Diff{Field: diffGroups, To: "docker, sudo"}),
		"other shell":   replaced(create, plan.Diff{Field: diffShell, To: "/bin/sh"}),
		"a uid":         append(slices.Clone(create), plan.Diff{Field: diffUID, To: "1500"}),
		"another user":  replaced(create, plan.Diff{Field: diffUser, To: "bob"}),
		"no keys shown": slices.DeleteFunc(slices.Clone(create), func(d plan.Diff) bool { return d.Field == diffKeysSHA }),
	} {
		if _, err := approvedUserChange(s, diffs, true); err == nil {
			t.Errorf("%s: granted", name)
		}
	}
	change := []plan.Diff{{Field: diffGroups, From: "adm", To: "adm, docker"}}
	if c, err := approvedUserChange(s, change, false); err != nil || !slices.Equal(c.Groups, []string{"docker"}) || c.Keys != nil || c.Create != nil {
		t.Errorf("%+v %v", c, err)
	}
	for name, diffs := range map[string][]plan.Diff{
		"a group not declared": {{Field: diffGroups, From: noGroups, To: "sudo"}},
		"a shell not declared": {{Field: diffShell, From: "/bin/sh", To: "/bin/bash"}},
		"other keys":           {{Field: diffKeysSHA, From: noFile, To: ContentSHA256("other")}},
		"a creation":           {{Field: diffUser, To: "app"}},
		"nothing":              {},
	} {
		if _, err := approvedUserChange(s, diffs, false); err == nil {
			t.Errorf("%s: granted", name)
		}
	}
}

func replaced(diffs []plan.Diff, d plan.Diff) []plan.Diff {
	out := slices.Clone(diffs)
	for i := range out {
		if out[i].Field == d.Field {
			out[i] = d
		}
	}
	return out
}

// Someone makes the user the plan saw missing, with keys of their own, before the apply: the
// creation's grant carries what the plan saw of authorized_keys (none), and mamelet takes nothing
// over (their keys stay, the user is not put in docker).
func TestAUserMadeSinceThePlanIsNotTakenOver(t *testing.T) {
	declared := userRes("web/user/app", `{"node":"web","name":"app","groups":["docker"],"authorizedKeys":["`+keyAlice+`"]}`)
	f, accounts := userFlow(t, declared)
	f.publish()
	f.node.round() // it says it takes linux
	f.publish()
	f.node.round() // it observes the user
	p, _ := f.plan()
	if c := f.userChange(); c.Status != plan.StatusChange || c.Operations[0].Op != OpCreateUser {
		t.Fatalf("the plan %+v", c)
	}
	addUser(t, f.node, appLine)
	write(t, f.node.root+"/home/app/.ssh/authorized_keys", keyBob+"\n")
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome == reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	part := partOf(f.revision())
	if u := part.user("app"); u == nil || u.Change == nil || u.Change.Before.Keys == nil || u.Change.Before.Keys.Exists {
		t.Fatalf("the grant %+v", u)
	}
	if len(accounts.commands) != 0 {
		t.Errorf("ran %q", accounts.commands)
	}
	if got := f.node.read("/home/app/.ssh/authorized_keys"); got != keyBob+"\n" {
		t.Errorf("their keys are now %q", got)
	}
}
