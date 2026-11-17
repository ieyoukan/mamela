package noderev

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

// testPart is a part of the shape an extension might give it: one grant.
type testPart struct {
	Grant *Once `json:"grant,omitempty"`
}

func init() {
	RegisterPart("test-ext", Part{
		Grants: func(raw jsontext.Value) ([]Grant, error) {
			var p testPart
			if err := json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)); err != nil {
				return nil, err
			}
			if p.Grant == nil {
				return nil, nil
			}
			return []Grant{{p.Grant.Operation, p.Grant.Attempt, p.Grant.Action, p.Grant.Expires}}, nil
		},
		ForMamelet: func(raw jsontext.Value, now time.Time) (jsontext.Value, error) {
			var p testPart
			if err := json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)); err != nil {
				return nil, err
			}
			p.Grant = HandOut(p.Grant, now)
			return json.Marshal(p, json.Deterministic(true))
		},
	})
}

func withPart(part string) Revision {
	r := sample()
	r.Ext = map[string]jsontext.Value{"test-ext": jsontext.Value(part)}
	return r
}

// An extension's grants are the core's: listed with the others, handed out only before their
// deadline and without it, and never signed with one.
func TestExtensionGrantsAreTheCores(t *testing.T) {
	r := withPart(`{"grant":{"operation":"op-aaaaaaaaaaaaaaaaaaaaaaaa","attempt":1,"action":"x","expires":"2026-11-10T10:00:00Z"}}`)
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	gs := r.Grants()
	if len(gs) == 0 || gs[len(gs)-1].Operation != "op-aaaaaaaaaaaaaaaaaaaaaaaa" || gs[len(gs)-1].Expires.IsZero() {
		t.Fatalf("grants %+v", gs)
	}
	if _, err := Sign(key(1), r); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("signed with a deadline: %v", err)
	}
	m := r.ForMamelet(t0)
	if got := string(m.Ext["test-ext"]); got != `{"grant":{"operation":"op-aaaaaaaaaaaaaaaaaaaaaaaa","attempt":1,"action":"x"}}` {
		t.Errorf("handed out %s", got)
	}
	if _, err := Sign(key(1), m); err != nil {
		t.Errorf("sign: %v", err)
	}
	if late := r.ForMamelet(t0.Add(2 * time.Hour)); string(late.Ext["test-ext"]) != `{}` {
		t.Errorf("handed out after the deadline: %s", late.Ext["test-ext"])
	}
}

// A part of an extension the Control Plane does not know is neither handed out nor signed, and the
// ext member's names and parts are checked.
func TestUnknownPartsAreNotHandedOut(t *testing.T) {
	r := sample()
	r.Ext = map[string]jsontext.Value{"other": jsontext.Value(`{}`)}
	if m := r.ForMamelet(t0); len(m.Ext) != 0 {
		t.Errorf("handed out %v", m.Ext)
	}
	if _, err := Sign(key(1), r); err == nil {
		t.Error("signed a part nobody knows")
	}
	for _, ext := range []map[string]jsontext.Value{{"Bad": jsontext.Value(`{}`)}, {"test-ext": jsontext.Value(`[]`)}, {"test-ext": jsontext.Value(`{`)}} {
		r.Ext = ext
		if err := r.Check(); err == nil {
			t.Errorf("%v checked", ext)
		}
	}
	if !strings.Contains(string(withPart(`{}`).Content()), `"ext":{"test-ext":{}}`) {
		t.Errorf("content %s", withPart(`{}`).Content())
	}
}

// The report of a mamelet older than the generic wire is read as before (the Control Plane decodes
// reports refusing unknown members, so it is updated before its mamelets), and a revision without
// ext is written as before (an older mamelet refuses a member it does not know).
func TestOlderMembersStillRead(t *testing.T) {
	var rep Report
	if err := json.Unmarshal([]byte(`{"node":"web","vmid":1,"serial":2,"version":"0.9.2","packages":[],"services":[],"results":[]}`), &rep, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if rep.Extensions != nil || rep.Ext != nil || rep.ExtRefused != nil {
		t.Errorf("%+v", rep)
	}
	payload, _ := json.Marshal(sample(), json.Deterministic(true))
	if strings.Contains(string(payload), "ext") {
		t.Errorf("%s", payload)
	}
}
