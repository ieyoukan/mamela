package linux

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// The operations of NodeUser, and their capabilities (what mamelet does with the grant).
const (
	OpCreateUser policy.Op = "linux.create-user"
	OpChangeUser policy.Op = "linux.change-user"

	CapCreateUser = "linux.create-user"
	CapChangeUser = "linux.change-user"
)

var createUserOp = operation{
	op: OpCreateUser,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0221",
		Message: "makes a user on the node (useradd) with the shell, home, groups and SSH keys of this plan",
		Hint:    "the user can log in with the keys of this plan, and do what its groups allow (a group such as sudo or docker makes it root)"},
	grant: grantUser,
}

var changeUserOp = operation{
	op: OpChangeUser,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0222",
		Message: "changes a user on the node: adds it to the groups, sets the shell and writes the SSH keys of this plan",
		Hint:    "read the keys this plan adds and takes out: logins with a key taken out stop working, and a group such as sudo or docker makes the user root"},
	grant: grantUser,
}

// userManifest is NodeUser in the node provider's manifest.
var userManifest = spec.Kind{Kind: KindUser, Operations: []spec.Operation{
	{Op: OpCreateUser, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapCreateUser, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "user {user}", Always: true,
		Conditions: node.GrantConditions(),
		Note: "mamelet runs useradd with the approved flags (never for root or uid 0), adds the user to the approved groups (usermod --append), " +
			"and writes its authorized_keys (0600 in ~/.ssh 0700, the user's, never through a symbolic link)",
		Reasons: []string{"mamelet makes the user once"},
	}}},
	{Op: OpChangeUser, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapChangeUser, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "user {user}", Always: true,
		Conditions: node.GrantConditions(),
		Note: "mamelet adds the user to the approved groups (usermod --append: it never takes one away), sets the approved shell (usermod --shell) " +
			"and writes the approved authorized_keys; it never changes a uid or a home",
		Reasons: []string{"mamelet changes the user once"},
	}}},
}}

// userEntry is what the part says of a user the declaration has (what mamelet reports on).
func userEntry(s *UserSpec) User {
	return User{Name: s.Name, Shell: s.CreateShell(), Groups: slices.Clone(s.Groups), UID: s.UID, Keys: s.AuthorizedKeys != nil}
}

// sameEntry is whether two entries of a user ask for the same (their changes aside).
func sameEntry(a, b User) bool {
	return a.Name == b.Name && a.Shell == b.Shell && slices.Equal(a.Groups, b.Groups) && a.UID == b.UID && a.Keys == b.Keys
}

// publishUsers adds to the part the users g declares on the node that it does not have yet (with no
// permission), and updates what it asks of the others. An approved change stays, unless the
// declaration of the user changed since: it was approved for another one.
func publishUsers(g *graph.Graph, name string, p *Part) {
	for _, r := range g.Sorted() {
		u := UserOf(r)
		if u == nil || u.Node != name {
			continue
		}
		e := userEntry(u)
		if cur := p.user(u.Name); cur != nil {
			if !sameEntry(*cur, e) {
				*cur = e
			}
		} else {
			p.Users = append(p.Users, e)
		}
	}
}

// pruneUsers takes out of the part the users g no longer declares on the node, with their changes.
// The users stay on the node.
func pruneUsers(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Resources {
		if u := UserOf(r); u != nil && u.Node == name {
			declared[u.Name] = true
		}
	}
	p.Users = slices.DeleteFunc(p.Users, func(u User) bool { return !declared[u.Name] })
}

// protectUsers takes every change out of the part of a protected node.
func protectUsers(p *Part) {
	for i := range p.Users {
		p.Users[i].Change = nil
	}
}

// grantUser grants exactly the change of a user the approved plan showed: how it is made, the
// groups it is added to, its shell and its keys, each as its diff says, or nothing.
func grantUser(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := UserOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	s := *want
	create := q.Step.Op == OpCreateUser
	c, err := approvedUserChange(&s, q.Step.Approved.Diffs, create)
	if err != nil {
		return node.Grant{}, false, fmt.Errorf("user %s on %s: %w (plan again)", s.Name, s.Node, err)
	}
	action, what := ActionChangeUser, "change user "+s.Name
	if create {
		action, what = ActionCreateUser, "create user "+s.Name
	}
	g := node.Grant{Node: s.Node, What: what}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		p := partOf(rev)
		change := c
		change.Grant = once(action)
		e := userEntry(&s)
		e.Change = &change
		if cur := p.user(s.Name); cur != nil {
			*cur = e
		} else {
			p.Users = append(p.Users, e)
		}
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}

// approvedUserChange is the change of user s that the approved diffs show, checked against the
// declaration: what the plan showed is what mamelet does.
func approvedUserChange(s *UserSpec, diffs []plan.Diff, create bool) (UserChange, error) {
	get := func(field string) (plan.Diff, bool) {
		i := slices.IndexFunc(diffs, func(d plan.Diff) bool { return d.Field == field })
		if i < 0 {
			return plan.Diff{}, false
		}
		return diffs[i], true
	}
	expect := func(field, want string) error {
		d, ok := get(field)
		switch {
		case want == "" && ok:
			return fmt.Errorf("the approved plan shows %s %q, which the declaration does not have", field, d.To)
		case want != "" && (!ok || d.To != want):
			return fmt.Errorf("the approved plan does not show %s %q", field, want)
		}
		return nil
	}
	var c UserChange
	keys := s.KeysContent()
	sum := ContentSHA256(keys)
	if create {
		home, made := s.CreateHome()
		c.Create = &UserCreate{System: s.System, UID: s.UID, Shell: s.CreateShell(), Home: home, CreateHome: made}
		c.Groups = slices.Clone(s.Groups)
		uid := ""
		if s.UID != 0 {
			uid = strconv.Itoa(s.UID)
		}
		keysSum := ""
		if s.AuthorizedKeys != nil {
			c.Keys, keysSum = &KeysWrite{Content: keys, SHA256: sum}, sum
		}
		for _, e := range []struct{ field, want string }{
			{diffUser, s.Name}, {diffUID, uid}, {diffShell, c.Create.Shell}, {diffGroups, strings.Join(s.Groups, ", ")}, {diffKeysSHA, keysSum},
		} {
			if err := expect(e.field, e.want); err != nil {
				return UserChange{}, err
			}
		}
		return c, nil
	}
	if _, ok := get(diffUser); ok {
		return UserChange{}, fmt.Errorf("the approved plan makes the user, which exists")
	}
	if d, ok := get(diffGroups); ok {
		before := strings.Split(d.From, ", ")
		for _, g := range strings.Split(d.To, ", ") {
			if slices.Contains(before, g) {
				continue
			}
			if !slices.Contains(s.Groups, g) {
				return UserChange{}, fmt.Errorf("the approved plan adds group %q, which the declaration does not have", g)
			}
			c.Groups = append(c.Groups, g)
		}
	}
	if d, ok := get(diffShell); ok {
		if s.Shell == "" || d.To != s.Shell {
			return UserChange{}, fmt.Errorf("the approved plan sets shell %q, which the declaration does not have", d.To)
		}
		c.Shell = s.Shell
	}
	if slices.ContainsFunc(diffs, func(d plan.Diff) bool {
		return d.Field == diffKeysSHA || d.Field == diffKeysFile || d.Field == diffSSHDir
	}) {
		if s.AuthorizedKeys == nil {
			return UserChange{}, fmt.Errorf("the approved plan writes authorized_keys, which the declaration does not have")
		}
		if d, ok := get(diffKeysSHA); ok && d.To != sum {
			return UserChange{}, fmt.Errorf("the SSH keys are not the ones the approved plan showed")
		}
		c.Keys = &KeysWrite{Content: keys, SHA256: sum}
	}
	if len(c.Groups) == 0 && c.Shell == "" && c.Keys == nil {
		return UserChange{}, fmt.Errorf("the approved plan changes nothing of the user")
	}
	return c, nil
}
