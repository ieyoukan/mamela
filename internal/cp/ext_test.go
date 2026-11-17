package cp

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

func init() {
	// An extension of the generic wire, for these tests: its part has no grant.
	noderev.RegisterPart("cp-test", noderev.Part{
		Grants:     func(jsontext.Value) ([]noderev.Grant, error) { return nil, nil },
		ForMamelet: func(p jsontext.Value, _ time.Time) (jsontext.Value, error) { return p, nil },
	})
}

// A node gets an extension's part only while its latest report says that its mamelet takes it: one
// that went back to an older release (which would refuse the whole revision) gets the revision
// without it, at once, before any plan publishes again.
func TestAPartOnlyForAMameletThatTakesIt(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Ext: map[string]jsontext.Value{"cp-test": jsontext.Value(`{"x":1}`)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func() map[string]jsontext.Value {
		t.Helper()
		s, err := c.Revision(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		r, err := noderev.Verify(e.pki.RevisionPublicKey(), s, noderev.Expectation{Node: "web", VMID: 7001, Now: e.now})
		if err != nil {
			t.Fatal(err)
		}
		return r.Ext
	}
	if ext := fetch(); len(ext) != 0 {
		t.Errorf("a node that has not reported got %s", ext)
	}
	if err := c.Report(e.ctx, noderev.Report{Node: "web", VMID: 7001, Extensions: map[string]int{"cp-test": 1}}); err != nil {
		t.Fatal(err)
	}
	if ext := fetch(); string(ext["cp-test"]) != `{"x":1}` {
		t.Errorf("a node that takes it got %s", ext)
	}
	if err := c.Report(e.ctx, noderev.Report{Node: "web", VMID: 7001, Version: "0.9.2"}); err != nil {
		t.Fatal(err)
	}
	if ext := fetch(); len(ext) != 0 {
		t.Errorf("a node that went back got %s", ext)
	}
}
