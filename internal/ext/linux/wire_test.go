package linux

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// The linux part of a revision, with grants as the Control Plane keeps them (each with its deadline).
func wireRevision(exp time.Time) noderev.Revision {
	r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 4, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour)}
	live := grant("/etc/a.conf", "a\n", opA, 1)
	live.Write.Grant.Expires = exp
	ended := grant("/etc/b.conf", "b\n", opB, 2)
	ended.Write.Grant.Expires = mt0.Add(-time.Minute)
	setPart(&r, Part{Files: []File{ended, live, {Path: "/etc/c.conf", Owner: "root", Group: "root"}}})
	return r
}

// The part is in the revision's content (so a changed part is a new serial), in a fixed form.
func TestPartIsInTheContent(t *testing.T) {
	r := wireRevision(mt0.Add(time.Hour))
	c := string(r.Content())
	if !strings.Contains(c, `"ext":{"linux":{"files":[{"path":"/etc/a.conf"`) || !strings.Contains(c, `"expires":"2026-11-17T10:00:00Z"`) {
		t.Errorf("content %s", c)
	}
	again := wireRevision(mt0.Add(time.Hour))
	if string(again.Content()) != c {
		t.Error("the same revision has another content")
	}
	other := wireRevision(mt0.Add(2 * time.Hour))
	if string(other.Content()) == c {
		t.Error("another deadline, the same content")
	}
	// Without an ext member a revision is what it was before the generic wire.
	plain := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 4}
	if strings.Contains(string(plain.Content()), "ext") {
		t.Errorf("%s", plain.Content())
	}
}

// The Control Plane's deadlines are never sent: ForMamelet takes out what has ended (with its
// content) and clears the rest, and Sign refuses a part that still has one.
func TestPartGrantsAsMameletGetsThem(t *testing.T) {
	r := wireRevision(mt0.Add(time.Hour))
	grants := r.Grants()
	if len(grants) != 2 || grants[0].Operation != opA || grants[0].Expires.IsZero() || grants[1].Operation != opB {
		t.Fatalf("grants %+v", grants)
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err := noderev.Sign(key, r); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("signed a part with deadlines: %v", err)
	}
	m := r.ForMamelet(mt0)
	p := partOf(&m)
	if len(p.Files) != 3 || p.Files[1].Write != nil || p.Files[0].Write == nil || !p.Files[0].Write.Grant.Expires.IsZero() {
		t.Fatalf("handed out %s", m.Ext[Name])
	}
	if strings.Contains(string(m.Ext[Name]), "b\\n") {
		t.Errorf("the content of an ended grant is handed out: %s", m.Ext[Name])
	}
	s, err := noderev.Sign(key, m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := noderev.Verify(key.Public().(ed25519.PublicKey), s, noderev.Expectation{Node: "web", VMID: 7001, Now: mt0})
	if err != nil || partOf(&got).Files[0].Write.Content != "a\n" {
		t.Errorf("verify: %v", err)
	}
	// After the deadline nothing is granted.
	late := r.ForMamelet(mt0.Add(2 * time.Hour))
	for _, f := range partOf(&late).Files {
		if f.Write != nil {
			t.Errorf("%s still granted", f.Path)
		}
	}
	// A grant without a deadline (one an older Control Plane would have made) is not handed out.
	old := wireRevision(time.Time{})
	if p := partOf(&[]noderev.Revision{old.ForMamelet(mt0)}[0]); p.Files[0].Write != nil {
		t.Error("a grant without a deadline was handed out")
	}
}

// A part of an extension this Control Plane does not know is never handed out nor signed; the ext
// member must be named and be objects.
func TestUnknownAndMalformedParts(t *testing.T) {
	r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour),
		Ext: map[string]jsontext.Value{"zzz": jsontext.Value(`{"grant":{"operation":"op-aaaaaaaaaaaaaaaaaaaaaaaa"}}`)}}
	if m := r.ForMamelet(mt0); len(m.Ext) != 0 {
		t.Errorf("handed out %s", m.Ext)
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err := noderev.Sign(key, r); err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Errorf("signed: %v", err)
	}
	for _, ext := range []map[string]jsontext.Value{
		{"Linux": jsontext.Value(`{}`)}, {"linux": jsontext.Value(`[]`)}, {"linux": jsontext.Value(`"x"`)}, {"linux": jsontext.Value(`{`)},
	} {
		r.Ext = ext
		if err := r.Check(); err == nil {
			t.Errorf("%s accepted", ext)
		}
	}
	// A part with a member this Control Plane does not know (a newer one wrote it) is not signed.
	r.Ext = map[string]jsontext.Value{Name: jsontext.Value(`{"users":[]}`)}
	if _, err := noderev.Sign(key, r); err == nil {
		t.Error("signed a part it cannot read")
	}
}

// What mamelet checks of a part before it uses it.
func TestCheckPart(t *testing.T) {
	ok := grant("/etc/a.conf", "a\n", opA, 1)
	if err := checkPart(encode(Part{Files: []File{ok}})); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(w *Write){
		"content":  func(w *Write) { w.Content = "changed" },
		"action":   func(w *Write) { w.Grant.Action = "install" },
		"op":       func(w *Write) { w.Grant.Operation = "op-1" },
		"attempt":  func(w *Write) { w.Grant.Attempt = 0 },
		"mode":     func(w *Write) { w.Mode = "4755" },
		"owner":    func(w *Write) { w.Owner = "-r" },
		"restart":  func(w *Write) { w.Restart = []string{"--now"} },
		"too long": func(w *Write) { w.Content = strings.Repeat("a", MaxContent+1); w.SHA256 = ContentSHA256(w.Content) },
	} {
		c := grant("/etc/a.conf", "a\n", opA, 1)
		f(c.Write)
		if err := checkPart(encode(Part{Files: []File{c}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkPart(encode(Part{Files: []File{ok, ok}})); err == nil {
		t.Error("a file twice")
	}
	if err := checkPart(jsontext.Value(`{"files":[],"users":[]}`)); err == nil {
		t.Error("an unknown member")
	}
}
