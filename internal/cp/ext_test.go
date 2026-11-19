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
		Version:    1,
		Grants:     func(jsontext.Value) ([]noderev.Grant, error) { return nil, nil },
		ForMamelet: func(p jsontext.Value, _ time.Time) (jsontext.Value, error) { return p, nil },
	})
	// One whose part is at version 2: a mamelet that reads only version 1 of it does not take it.
	noderev.RegisterPart("cp-test-two", noderev.Part{
		Version:    2,
		Grants:     func(jsontext.Value) ([]noderev.Grant, error) { return nil, nil },
		ForMamelet: func(p jsontext.Value, _ time.Time) (jsontext.Value, error) { return p, nil },
	})
}

// fetchExt is the ext member of the revision the mamelet of cfg fetches.
func (e *env) fetchExt(cfg mamelet.Config) map[string]jsontext.Value {
	e.t.Helper()
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := c.Revision(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := noderev.Verify(e.pki.RevisionPublicKey(), s, noderev.Expectation{Node: "web", VMID: 7001, Now: e.now})
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Ext
}

func (e *env) report(cfg mamelet.Config, r noderev.Report) {
	e.t.Helper()
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := c.Report(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

// The version of a part decides, as for publishing and granting (noderev.Takes): a mamelet that lists
// an extension at an older version than the part's gets the revision without it.
func TestAPartOnlyAtItsVersion(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Ext: map[string]jsontext.Value{"cp-test": jsontext.Value(`{"x":1}`), "cp-test-two": jsontext.Value(`{"y":2}`)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, Extensions: map[string]int{"cp-test": 1, "cp-test-two": 1}})
	if ext := e.fetchExt(cfg); len(ext) != 1 || ext["cp-test"] == nil {
		t.Errorf("version 1 of cp-test-two: %s", ext)
	}
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, Extensions: map[string]int{"cp-test-two": 3}})
	if ext := e.fetchExt(cfg); len(ext) != 1 || ext["cp-test-two"] == nil {
		t.Errorf("a later version of cp-test-two: %s", ext)
	}
}

// A report from before the node's current registration says what another mamelet took: a mamelet
// enrolled again (an explicit downgrade, a new machine of the name) gets no part before it reports.
func TestAPartNotForAnEarlierRegistration(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Ext: map[string]jsontext.Value{"cp-test": jsontext.Value(`{"x":1}`)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, Extensions: map[string]int{"cp-test": 1}})
	if ext := e.fetchExt(cfg); ext["cp-test"] == nil {
		t.Fatalf("the mamelet that takes it: %s", ext)
	}
	e.now = e.now.Add(time.Minute)
	again := e.enroll("web", 7001)
	if ext := e.fetchExt(again); len(ext) != 0 {
		t.Errorf("a mamelet enrolled after the report got %s", ext)
	}
	e.report(again, noderev.Report{Node: "web", VMID: 7001, Extensions: map[string]int{"cp-test": 1}})
	if ext := e.fetchExt(again); ext["cp-test"] == nil {
		t.Errorf("once it reports: %s", ext)
	}
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

// While an update of mamelet is handed out and its binary has not reported, no part is handed out:
// the mamelet that fetches may be the new one, of a release that may not take the extension (a
// downgrade), and would refuse the whole revision for it. Once the new binary reports, its report
// decides; so does the old one's once the update has ended.
func TestNoPartWhileAMameletUpdateIsUnderWay(t *testing.T) {
	const cur, next = "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Ext: map[string]jsontext.Value{"cp-test": jsontext.Value(`{"x":1}`)},
			Mamelet: &noderev.MameletUpdate{SHA256: next, Once: noderev.Once{Operation: "op-aaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1, Action: noderev.ActionUpdate,
				Expires: e.now.Add(10 * time.Minute)}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, MameletSHA256: cur, Extensions: map[string]int{"cp-test": 1}})
	if ext := e.fetchExt(cfg); len(ext) != 0 {
		t.Errorf("during the update: %s", ext)
	}
	// The new binary reports, and takes the extension.
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, MameletSHA256: next, Extensions: map[string]int{"cp-test": 1}})
	if ext := e.fetchExt(cfg); ext["cp-test"] == nil {
		t.Errorf("the new binary that takes it: %s", ext)
	}
	// The update did not happen, and its grant has ended: the old binary's report decides again.
	e.report(cfg, noderev.Report{Node: "web", VMID: 7001, MameletSHA256: cur, Extensions: map[string]int{"cp-test": 1}})
	e.now = e.now.Add(11 * time.Minute)
	if ext := e.fetchExt(cfg); ext["cp-test"] == nil {
		t.Errorf("after the update's grant ended: %s", ext)
	}
}
