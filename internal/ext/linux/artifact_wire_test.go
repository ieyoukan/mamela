package linux

import (
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// What mamelet checks of a part's artifacts before it uses it.
func TestCheckPartArtifacts(t *testing.T) {
	ok := install("/usr/local/bin/tool", "https://example.com/tool", sumA, "", opA, 1)
	if err := checkPart(encode(Part{Artifacts: []Artifact{ok}})); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(in *Install){
		"action":  func(in *Install) { in.Grant.Action = ActionWrite },
		"attempt": func(in *Install) { in.Grant.Attempt = 0 },
		"http":    func(in *Install) { in.URL = "http://example.com/tool" },
		"user":    func(in *Install) { in.URL = "https://me:x@example.com/tool" },
		"sha256":  func(in *Install) { in.SHA256 = "abc" },
		"extract": func(in *Install) { in.Extract = "../etc/shadow" },
		"mode":    func(in *Install) { in.Mode = "4755" },
		"owner":   func(in *Install) { in.Owner = "-r" },
		"restart": func(in *Install) { in.Restart = []string{"--now"} },
	} {
		c := install("/usr/local/bin/tool", "https://example.com/tool", sumA, "", opA, 1)
		f(c.Install)
		if err := checkPart(encode(Part{Artifacts: []Artifact{c}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkPart(encode(Part{Artifacts: []Artifact{ok, ok}})); err == nil {
		t.Error("an artifact twice")
	}
	if err := checkPart(encode(Part{Files: []File{grant("/usr/local/bin/tool", "x", opB, 1)}, Artifacts: []Artifact{ok}})); err == nil || !strings.Contains(err.Error(), "a file's path too") {
		t.Errorf("a file and an artifact at one path: %v", err)
	}
}

// An installation's grant is handed out like a file's: without its deadline, and not after it.
func TestArtifactGrantsAsMameletGetsThem(t *testing.T) {
	r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 4, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour)}
	live := install("/usr/local/bin/a", "https://example.com/a", sumA, "", opA, 1)
	live.Install.Grant.Expires = mt0.Add(time.Hour)
	ended := install("/usr/local/bin/b", "https://example.com/b", sumB, "", opB, 1)
	ended.Install.Grant.Expires = mt0.Add(-time.Minute)
	setPart(&r, Part{Artifacts: []Artifact{ended, live}})
	if !strings.Contains(string(r.Content()), `"ext":{"linux":{"artifacts":[{"path":"/usr/local/bin/a","owner":"root","group":"root","install":{"url":"https://example.com/a"`) {
		t.Errorf("content %s", r.Content())
	}
	if g := r.Grants(); len(g) != 2 || g[0].Operation != opA || g[1].Operation != opB || g[0].Expires.IsZero() {
		t.Fatalf("grants %+v", g)
	}
	m := r.ForMamelet(mt0)
	p := partOf(&m)
	if len(p.Artifacts) != 2 || p.Artifacts[0].Install == nil || !p.Artifacts[0].Install.Grant.Expires.IsZero() || p.Artifacts[1].Install != nil {
		t.Errorf("handed out %s", m.Ext[Name])
	}
	// An artifact alone keeps the part; none takes it out.
	setPart(&r, Part{})
	if len(r.Ext) != 0 {
		t.Errorf("an empty part stays: %s", r.Ext)
	}
}
