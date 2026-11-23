package linux

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/site"
)

// An nginx site and nginx's own configuration, each checked by nginx -t and reloading nginx: the
// declarations of the tests of checks.
const (
	siteConf  = "server {\n  listen 80;\n  root /srv/www;\n}\n"
	nginxConf = "events {}\nhttp {\n  include /etc/nginx/conf.d/*.conf;\n}\n"
)

var (
	siteFile  = fileRes("web/file/etc/nginx/conf.d/site.conf", `{"node":"web","path":"/etc/nginx/conf.d/site.conf","content":"server {\n  listen 80;\n  root /srv/www;\n}\n","check":"nginx","reload":["nginx"]}`)
	nginxFile = fileRes("web/file/etc/nginx/nginx.conf", `{"node":"web","path":"/etc/nginx/nginx.conf","content":"events {}\nhttp {\n  include /etc/nginx/conf.d/*.conf;\n}\n","check":"nginx","reload":["nginx"]}`)
	nginxSvc  = `{"kind":"NodeService","name":"web/nginx","spec":{"node":"web","service":"nginx","enabled":true,"running":true}}`
)

// A check and the services reloaded are in the graph, and its digest: a declaration without them is
// the graph it was before them.
func TestFileCheckAndReloadInTheGraph(t *testing.T) {
	g, err := decodeGraph(siteFile)
	if err != nil {
		t.Fatal(err)
	}
	f := FileOf(g.Resources[1])
	if f.Check != "nginx" || !slices.Equal(f.Reload, []string{"nginx"}) {
		t.Fatalf("%+v", f)
	}
	if want := `"check":"nginx"`; !strings.Contains(string(g.Canonical()), want) || !strings.Contains(string(g.Canonical()), `"reload":["nginx"]`) {
		t.Errorf("canonical %s", g.Canonical())
	}
	plain, err := decodeGraph(fileRes("web/file/etc/nginx/conf.d/site.conf", `{"node":"web","path":"/etc/nginx/conf.d/site.conf","content":"server {\n  listen 80;\n  root /srv/www;\n}\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Digest() == g.Digest() || strings.Contains(string(plain.Canonical()), "check") || strings.Contains(string(plain.Canonical()), "reload") {
		t.Errorf("without a check: %s", plain.Canonical())
	}
	// A service reloaded that the declaration does not have: the warning of a restart.
	if w := graphWarnings(g); len(w) != 1 || w[0].Code != CodeRestartUndeclared || !strings.Contains(w[0].Message, "reloads service nginx") {
		t.Errorf("warnings %+v", w)
	}
}

// A check that mamelet does not have, a service both restarted and reloaded, and the other mistakes of
// a check or a reload are refused, with what to write instead.
func TestFileCheckRejections(t *testing.T) {
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"unknown check", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","check":"apache2"}`), graph.CodeInvalidValue, `"apache2" is not a check mamelet runs`},
		{"unknown check hint", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","check":"nginx -t"}`), graph.CodeInvalidValue, `"nginx" (nginx -t)`},
		{"a command", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","check":"/bin/sh -c true"}`), graph.CodeInvalidValue, "not a check mamelet runs"},
		{"empty check", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","check":""}`), graph.CodeInvalidValue, "check is empty"},
		{"check not a string", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","check":["nginx"]}`), graph.CodeWrongType, "check"},
		{"restart and reload", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","restart":["nginx"],"reload":["nginx.service"]}`), graph.CodeInvalidValue, "both in restart and in reload"},
		{"reload not a list", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","reload":"nginx"}`), graph.CodeWrongType, "reload"},
		{"null reload", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","reload":null}`), graph.CodeWrongType, "reload"},
		{"reload name", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","reload":["-x"]}`), graph.CodeInvalidValue, "service name"},
		{"reload twice", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","reload":["nginx","nginx.service"]}`), graph.CodeInvalidValue, "twice"},
		{"reload mamelet", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","reload":["mamelet"]}`), graph.CodeInvalidValue, "take mamelet out of reload"},
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
}

// mamelet checks the write of a revision as the graph checks the declaration: a check it does not
// have, a service both restarted and reloaded, a reload of mamelet, whatever the revision says.
func TestMameletRefusesAWriteWithAnUnknownCheck(t *testing.T) {
	base := grant("/etc/nginx/conf.d/site.conf", siteConf, opA, 1).Write
	for name, change := range map[string]func(w *Write){
		"unknown check":      func(w *Write) { w.Check = "sh" },
		"restart and reload": func(w *Write) { w.Restart, w.Reload = []string{"nginx"}, []string{"nginx"} },
		"reload mamelet":     func(w *Write) { w.Reload = []string{"mamelet"} },
		"reload name":        func(w *Write) { w.Reload = []string{"-x"} },
	} {
		w := *base
		change(&w)
		if err := checkWrite(w); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w := *base
	w.Check, w.Reload = "nginx", []string{"nginx"}
	if err := checkWrite(w); err != nil {
		t.Errorf("a valid write: %v", err)
	}
}

// Every check mamelet runs is a program it is allowed to run (an absolute path, compiled in), with
// fixed arguments, and nothing at its path or by its name in the PATH is a file Mamela writes.
func TestEveryCheckIsAFixedProgram(t *testing.T) {
	for _, v := range validators {
		if !strings.HasPrefix(v.Program, "/") || len(v.Args) == 0 || v.Shown == "" {
			t.Errorf("%+v", v)
		}
		if CheckPath(v.Program) == nil {
			t.Errorf("%s, a check's program, may be written", v.Program)
		}
		for _, d := range pathDirs {
			if q := d + "/" + v.Program[strings.LastIndex(v.Program, "/")+1:]; CheckPath(q) == nil {
				t.Errorf("%s may be written", q)
			}
		}
	}
	if !slices.IsSortedFunc(validators, func(a, b validator) int { return strings.Compare(a.Name, b.Name) }) {
		t.Error("the checks are not sorted by name")
	}
}

// The golden plans of the checks: each scenario as in TestGoldenFilePlans (testdata/golden/plan/check-*).
func TestGoldenFileCheckPlans(t *testing.T) {
	root := accountsOf("root")
	now := world{mamelet: Release + "+abc", accepts: Version, services: map[string][2]bool{"nginx": {true, true}}}
	with := func(w world, f func(w *world)) world { f(&w); return w }
	reported := func(files ...FileState) *Observed { return &Observed{Files: files, Users: root, Groups: root} }
	old := present("/etc/nginx/conf.d/site.conf", "server { listen 81; }\n")
	held := policy.Default
	held.ObserveOnly = []string{"web"}

	for _, s := range []struct {
		name      string
		resources []string
		world     world
		pol       policy.Policy
		status    plan.Status
		code      string // of the site's note ("" none)
	}{
		// The approver sees the check and the services reloaded.
		{"check-create", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			w.observed = reported(FileState{Path: "/etc/nginx/conf.d/site.conf"})
		}), policy.Default, plan.StatusChange, ""},
		{"check-change", []string{siteFile, nginxFile, nginxSvc}, with(now, func(w *world) {
			w.observed = reported(old, present("/etc/nginx/nginx.conf", "events {}\n"))
		}), policy.Default, plan.StatusChange, ""},
		// The last write of this content was put back: the check failed.
		{"check-rolled-back", []string{siteFile, nginxFile, nginxSvc}, with(now, func(w *world) {
			site := old
			site.RolledBack = &RolledBack{SHA256: ContentSHA256(siteConf), Check: "nginx", Output: `nginx: [emerg] unknown directive "rot" in /etc/nginx/conf.d/site.conf:3`,
				Files: []string{"/etc/nginx/conf.d/site.conf", "/etc/nginx/nginx.conf"}}
			w.observed = reported(site, present("/etc/nginx/nginx.conf", nginxConf))
		}), policy.Default, plan.StatusChange, codeFileRolledBack},
		// A record of a put back of another content (the declaration was fixed since) says nothing.
		{"check-rolled-back-fixed", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			site := old
			site.RolledBack = &RolledBack{SHA256: ContentSHA256("server { rot /srv; }\n"), Check: "nginx", Output: "nginx: [emerg] unknown directive"}
			w.observed = reported(site)
		}), policy.Default, plan.StatusChange, ""},
		// Written, its check not run yet: mamelet runs it when it starts again.
		{"check-unfinished", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			site := present("/etc/nginx/conf.d/site.conf", siteConf)
			site.Unchecked = &Unchecked{SHA256: ContentSHA256(siteConf), Check: "nginx"}
			w.observed = reported(site)
		}), policy.Default, plan.StatusWaiting, codeFileUnchecked},
		// The check failed, and mamelet stopped before it had put the file back: it does when it starts.
		{"check-put-back-unfinished", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			site := present("/etc/nginx/conf.d/site.conf", siteConf)
			site.Unchecked = &Unchecked{SHA256: ContentSHA256(siteConf), Check: "nginx", Failed: "nginx: [emerg] unknown directive"}
			w.observed = reported(site)
		}), policy.Default, plan.StatusWaiting, codeFileUnchecked},
		// The check failed, and mamelet cannot put the file back.
		{"check-stuck", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			site := present("/etc/nginx/conf.d/site.conf", siteConf)
			site.Unchecked = &Unchecked{SHA256: ContentSHA256(siteConf), Check: "nginx", Failed: "nginx: [emerg] unknown directive",
				Error: "put back /etc/nginx/conf.d/site.conf: no space left on device"}
			w.observed = reported(site)
		}), policy.Default, plan.StatusBlocked, codeFileCheckStuck},
		// mamelet lost its record of its checks: a file with a check is blocked, one without is not.
		{"check-record-lost", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			w.observed = reported(present("/etc/nginx/conf.d/site.conf", siteConf))
			w.observed.SetAside = []string{checksName + corruptSuffix}
		}), policy.Default, plan.StatusBlocked, codeFileCheckStuck},
		// Written and checked, but its reload did not finish.
		{"reload-pending", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			site := present("/etc/nginx/conf.d/site.conf", siteConf)
			site.Pending = &Pending{SHA256: ContentSHA256(siteConf), Reloads: []string{"nginx"}}
			w.observed = reported(site)
		}), policy.Default, plan.StatusChange, codePending},
		// Observe-only: the change is shown, and denied.
		{"check-observe-only", []string{siteFile, nginxSvc}, with(now, func(w *world) {
			w.observed = reported(old)
		}), held, plan.StatusChange, ""},
	} {
		t.Run(s.name, func(t *testing.T) {
			g, err := decodeGraph(s.resources...)
			if err != nil {
				t.Fatal(err)
			}
			snap := s.world.snapshot(g)
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
			c := p.Changes[slices.IndexFunc(p.Changes, func(c plan.Change) bool { return c.Name == "web/file/etc/nginx/conf.d/site.conf" })]
			if c.Status != s.status || (c.Note == nil) != (s.code == "") || c.Note != nil && c.Note.Code != s.code {
				t.Fatalf("the site: %s %+v", c.Status, c.Note)
			}
			if c.Status != plan.StatusChange {
				return
			}
			// The approver sees what runs after the write, before approving it.
			text := r.String()
			for _, want := range []string{"check: nginx -t (once the files of this node with this check are written; if it fails, they are put back", "reload: nginx (each only if it is active: systemctl reload)"} {
				if !strings.Contains(text, want) {
					t.Errorf("the plan does not show %q:\n%s", want, text)
				}
			}
			switch {
			case s.pol.Holds("web"):
				if c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0312" {
					t.Errorf("observe-only: %+v", c)
				}
			case c.Verdict != policy.ApprovalRequired || !slices.ContainsFunc(c.Operations, func(o plan.Operation) bool { return o.Op == OpWriteFile }):
				t.Errorf("a file is written without an approval: %+v", c)
			}
		})
	}
}
