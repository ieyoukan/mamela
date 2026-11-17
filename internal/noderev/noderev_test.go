package noderev

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func key(seed byte) ed25519.PrivateKey {
	s := make([]byte, ed25519.SeedSize)
	s[0] = seed
	return ed25519.NewKeyFromSeed(s)
}

func sample() Revision {
	return Revision{Schema: Schema, Node: "web", VMID: 7001, Serial: 3, IssuedAt: t0, ExpiresAt: t0.Add(24 * time.Hour),
		Packages: []Package{{Name: "nginx", Install: &Install{Operation: "op-0123456789abcdef01234567", Attempt: 1,
			ChangeSet: "sha256:" + strings.Repeat("a", 64)}}},
		Services: []Service{{Name: "nginx.service", Enabled: true, Running: true, Repair: []string{"start"},
			Once: []Once{{Operation: "op-0123456789abcdef01234568", Attempt: 1, Action: "enable"}}}}}
}

func expect() Expectation {
	return Expectation{Node: "web", VMID: 7001, Now: t0.Add(time.Hour), LastSerial: 2}
}

func TestSignAndVerify(t *testing.T) {
	k := key(1)
	s, err := Sign(k, sample())
	if err != nil {
		t.Fatal(err)
	}
	r, err := Verify(k.Public().(ed25519.PublicKey), s, expect())
	if err != nil {
		t.Fatal(err)
	}
	if r.Serial != 3 || r.Packages[0].Install.ChangeSet != sample().Packages[0].Install.ChangeSet {
		t.Fatalf("got %+v", r)
	}
}

func TestVerifyRefuses(t *testing.T) {
	k := key(1)
	pub := k.Public().(ed25519.PublicKey)
	good, err := Sign(k, sample())
	if err != nil {
		t.Fatal(err)
	}
	signedWith := func(edit func(*Revision)) Signed {
		r := sample()
		edit(&r)
		s, err := Sign(k, r)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tampered := good
	tampered.Payload = []byte(strings.Replace(string(good.Payload), `"nginx"`, `"nginy"`, 1))

	for name, tc := range map[string]struct {
		key  ed25519.PublicKey
		s    Signed
		want Expectation
		err  error
	}{
		"other key":    {key(2).Public().(ed25519.PublicKey), good, expect(), ErrBadSignature},
		"tampered":     {pub, tampered, expect(), ErrBadSignature},
		"no signature": {pub, Signed{Payload: good.Payload}, expect(), ErrBadSignature},
		"other node":   {pub, good, Expectation{Node: "db", VMID: 7001, Now: t0}, ErrOtherNode},
		"other vmid":   {pub, good, Expectation{Node: "web", VMID: 7002, Now: t0}, ErrOtherNode},
		"expired":      {pub, good, Expectation{Node: "web", VMID: 7001, Now: t0.Add(24 * time.Hour)}, ErrExpired},
		"future":       {pub, good, Expectation{Node: "web", VMID: 7001, Now: t0.Add(-time.Hour)}, ErrFromFuture},
		"rolled back":  {pub, good, Expectation{Node: "web", VMID: 7001, Now: t0, LastSerial: 4}, ErrRolledBack},
		"same serial is fine": {pub, signedWith(func(r *Revision) {}), Expectation{Node: "web", VMID: 7001, Now: t0,
			LastSerial: 3}, nil},
	} {
		_, err := Verify(tc.key, tc.s, tc.want)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: got %v, want %v", name, err, tc.err)
		}
	}
}

// Anything that could become a command-line option or a path, and any unknown action, makes the
// revision invalid, even when it is correctly signed.
func TestContentIsChecked(t *testing.T) {
	k := key(1)
	for name, edit := range map[string]func(*Revision){
		"option as package":   func(r *Revision) { r.Packages[0].Name = "-oDpkg::Pre-Invoke=sh" },
		"space in package":    func(r *Revision) { r.Packages[0].Name = "nginx curl" },
		"path as service":     func(r *Revision) { r.Services[0].Name = "../../etc/x" },
		"option as service":   func(r *Revision) { r.Services[0].Name = "--now" },
		"unknown action":      func(r *Revision) { r.Services[0].Once[0].Action = "restart" },
		"unknown repair":      func(r *Revision) { r.Services[0].Repair = []string{"restart"} },
		"bad change set":      func(r *Revision) { r.Packages[0].Install.ChangeSet = "anything" },
		"bad operation":       func(r *Revision) { r.Packages[0].Install.Operation = "x" },
		"no attempt":          func(r *Revision) { r.Services[0].Once[0].Attempt = 0 },
		"duplicate package":   func(r *Revision) { r.Packages = append(r.Packages, Package{Name: "nginx"}) },
		"bad node":            func(r *Revision) { r.Node = "Web_1" },
		"no validity":         func(r *Revision) { r.ExpiresAt = r.IssuedAt },
		"other schema":        func(r *Revision) { r.Schema = "mamelet-revision/v2" },
		"zero serial":         func(r *Revision) { r.Serial = 0 },
		"no vmid":             func(r *Revision) { r.VMID = 0 },
		"option in once name": func(r *Revision) { r.Services[0].Name = "-x" },
	} {
		r := sample()
		edit(&r)
		if _, err := Sign(k, r); err == nil {
			t.Errorf("%s: signed an invalid revision", name)
		}
	}

	// A valid signature over an invalid payload is refused too, as are unknown members.
	for name, payload := range map[string]string{
		"unknown member": strings.Replace(string(mustSign(t, k).Payload), `"node":`, `"shell":"rm -rf /","node":`, 1),
		"invalid":        strings.Replace(string(mustSign(t, k).Payload), `"enable"`, `"mask"`, 1),
	} {
		s := Signed{Payload: []byte(payload), Signature: ed25519.Sign(k, []byte(payload))}
		if _, err := Verify(k.Public().(ed25519.PublicKey), s, expect()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustSign(t *testing.T, k ed25519.PrivateKey) Signed {
	t.Helper()
	s, err := Sign(k, sample())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContentIgnoresValidityAndOrder(t *testing.T) {
	a := sample()
	b := sample()
	b.IssuedAt, b.ExpiresAt = t0.Add(time.Hour), t0.Add(48*time.Hour)
	b.Packages = append([]Package{{Name: "curl"}}, b.Packages...)
	a.Packages = append(a.Packages, Package{Name: "curl"})
	if string(a.Content()) != string(b.Content()) {
		t.Fatalf("content differs:\n%s\n%s", a.Content(), b.Content())
	}
	b.Services[0].Running = false
	if string(a.Content()) == string(b.Content()) {
		t.Fatal("content does not cover the services")
	}
}

func TestChangeSetDigest(t *testing.T) {
	a := NewChangeSet([]Change{
		{Action: ChangeInstall, Package: "nginx", Version: "1.24.0-2ubuntu7.4", Origins: []string{"Ubuntu:24.04/noble-updates", "Ubuntu:24.04/noble-security"}},
		{Action: ChangeInstall, Package: "nginx-common", Version: "1.24.0-2ubuntu7.4", Origins: []string{"Ubuntu:24.04/noble-updates"}},
	})
	b := NewChangeSet([]Change{
		{Action: ChangeInstall, Package: "nginx-common", Version: "1.24.0-2ubuntu7.4", Origins: []string{"Ubuntu:24.04/noble-updates"}},
		{Action: ChangeInstall, Package: "nginx", Version: "1.24.0-2ubuntu7.4", Origins: []string{"Ubuntu:24.04/noble-security", "Ubuntu:24.04/noble-updates"}},
	})
	if a.Digest != b.Digest || a.Check() != nil || a.Removes() {
		t.Fatalf("digests %s %s", a.Digest, b.Digest)
	}
	c := NewChangeSet(append(a.Changes, Change{Action: ChangeRemove, Package: "apache2"}))
	if c.Digest == a.Digest || !c.Removes() {
		t.Fatal("a removal does not change the digest")
	}
	forged := a
	forged.Changes = c.Changes
	if forged.Check() == nil {
		t.Fatal("a change set whose digest does not match was accepted")
	}
}

func TestManifestsAreChecked(t *testing.T) {
	rev := func(m ...Manifest) Revision {
		return Revision{Schema: Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: time.Unix(1, 0), ExpiresAt: time.Unix(100, 0), Manifests: m}
	}
	ok := Manifest{Name: "argocd", Content: "kind: Namespace\n", Write: &Once{Operation: "op-000000000000000000000001", Attempt: 1, Action: ActionWrite}}
	if r := rev(ok); r.Check() != nil {
		t.Fatalf("a valid manifest: %v", r.Check())
	}
	for name, r := range map[string]Revision{
		"a path as the name": rev(Manifest{Name: "../x", Content: "a"}),
		"an empty content":   rev(Manifest{Name: "x", Content: ""}),
		"too large":          rev(Manifest{Name: "x", Content: strings.Repeat("a", MaxManifest+1)}),
		"twice":              rev(ok, ok),
		"another action":     rev(Manifest{Name: "x", Content: "a", Write: &Once{Operation: "op-000000000000000000000001", Attempt: 1, Action: ActionInstall}}),
		"attempt 0":          rev(Manifest{Name: "x", Content: "a", Write: &Once{Operation: "op-000000000000000000000001", Action: ActionWrite}}),
	} {
		if r.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNodeRemovalIsChecked(t *testing.T) {
	tok := strings.Repeat("a", 64)
	rev := func(role string, rm *NodeRemoval) Revision {
		k := &K3s{Version: "v1.37.2+k3s1", SHA256: tok, Role: role, Token: tok, Remove: rm}
		if role == K3sAgent {
			k.Server = "https://192.0.2.10:6443"
		}
		return Revision{Schema: Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: time.Unix(1, 0), ExpiresAt: time.Unix(100, 0), K3s: k}
	}
	once := Once{Operation: "op-000000000000000000000001", Attempt: 1, Action: ActionRemove}
	if r := rev(K3sServer, &NodeRemoval{Node: "agent-1", Once: once}); r.Check() != nil {
		t.Fatalf("a valid removal: %v", r.Check())
	}
	bad := once
	bad.Action = ActionInstall
	for name, r := range map[string]Revision{
		"an agent removes nodes": rev(K3sAgent, &NodeRemoval{Node: "agent-1", Once: once}),
		"an argument as a name":  rev(K3sServer, &NodeRemoval{Node: "--all", Once: once}),
		"a path as a name":       rev(K3sServer, &NodeRemoval{Node: "../x", Once: once}),
		"another action":         rev(K3sServer, &NodeRemoval{Node: "agent-1", Once: bad}),
		"no operation":           rev(K3sServer, &NodeRemoval{Node: "agent-1", Once: Once{Attempt: 1, Action: ActionRemove}}),
	} {
		if r.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMameletUpdateIsChecked(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	rev := func(u *MameletUpdate) Revision {
		return Revision{Schema: Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: time.Unix(1, 0), ExpiresAt: time.Unix(100, 0), Mamelet: u}
	}
	once := Once{Operation: "op-000000000000000000000001", Attempt: 1, Action: ActionUpdate}
	if r := rev(&MameletUpdate{SHA256: sha, Once: once}); r.Check() != nil {
		t.Fatalf("a valid update: %v", r.Check())
	}
	bad := once
	bad.Action = ActionInstall
	noAttempt := once
	noAttempt.Attempt = 0
	for name, r := range map[string]Revision{
		"a short sha256":   rev(&MameletUpdate{SHA256: "abcd", Once: once}),
		"an uppercase one": rev(&MameletUpdate{SHA256: strings.ToUpper(sha), Once: once}),
		"another action":   rev(&MameletUpdate{SHA256: sha, Once: bad}),
		"attempt 0":        rev(&MameletUpdate{SHA256: sha, Once: noAttempt}),
		"no operation":     rev(&MameletUpdate{SHA256: sha, Once: Once{Attempt: 1, Action: ActionUpdate}}),
	} {
		if r.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A grant is part of the content: a revision with one is not the revision without.
	if string(rev(nil).Content()) == string(rev(&MameletUpdate{SHA256: sha, Once: once}).Content()) {
		t.Error("the update grant is not in the content")
	}
}

// allGrants is a revision with every kind of one-time grant, each with the deadline the Control
// Plane gave it.
func allGrants(deadline time.Time) Revision {
	op := func(n string) string { return "op-0123456789abcdef012345" + n }
	r := sample()
	r.Packages[0].Install.Expires = deadline
	r.Services[0].Once[0].Expires = deadline
	r.Manifests = []Manifest{{Name: "argocd", Content: "kind: Namespace\n", Write: &Once{Operation: op("10"), Attempt: 1, Action: ActionWrite, Expires: deadline}}}
	r.K3s = &K3s{Version: "v1.33.5+k3s1", SHA256: strings.Repeat("b", 64), Role: K3sServer, Token: strings.Repeat("c", 64),
		Install: &Once{Operation: op("11"), Attempt: 1, Action: ActionInstall, Expires: deadline},
		Remove:  &NodeRemoval{Node: "gone", Once: Once{Operation: op("12"), Attempt: 1, Action: ActionRemove, Expires: deadline}}}
	r.Mamelet = &MameletUpdate{SHA256: strings.Repeat("d", 64), Once: Once{Operation: op("13"), Attempt: 1, Action: ActionUpdate, Expires: deadline}}
	r.ControlPlane = "https://cp.example:8443"
	return r
}

// A one-time grant is handed out until its deadline, and never after (audit A2).
func TestForMamelet(t *testing.T) {
	deadline := t0.Add(30 * time.Minute)
	full := allGrants(deadline)
	if n := len(full.Grants()); n != 6 {
		t.Fatalf("the revision has %d grants, the test means every kind (6): %v", n, full.Grants())
	}
	live := full.ForMamelet(t0)
	if n := len(live.Grants()); n != 6 {
		t.Fatalf("before the deadline %d of 6 grants are handed out", n)
	}
	for _, g := range live.Grants() {
		if !g.Expires.IsZero() {
			t.Errorf("the deadline of %s is sent", g.Operation)
		}
	}
	for name, at := range map[string]time.Time{"at the deadline": deadline, "after": deadline.Add(24 * time.Hour)} {
		gone := full.ForMamelet(at)
		if n := len(gone.Grants()); n != 0 {
			t.Errorf("%s: %d grants are still handed out: %s", name, n, gone.Content())
		}
		// Everything else is as stored: the same serial, and what mamelet watches and may repair.
		if gone.Serial != full.Serial || gone.Packages[0].Name != "nginx" || !strings.Contains(string(gone.Content()), `"repair":["start"]`) ||
			gone.K3s == nil || gone.K3s.Token != full.K3s.Token || gone.Manifests[0].Content != full.Manifests[0].Content ||
			gone.ControlPlane != full.ControlPlane {
			t.Errorf("%s: more than the grants changed: %s", name, gone.Content())
		}
	}
	// A grant without a deadline (an older Control Plane issued it) is not handed out.
	if n := len(allGrants(time.Time{}).ForMamelet(t0).Grants()); n != 0 {
		t.Errorf("%d grants without a deadline are handed out", n)
	}
	// ForMamelet does not change the stored revision.
	if full.Packages[0].Install == nil || full.Packages[0].Install.Expires.IsZero() || full.K3s.Remove == nil || full.Mamelet == nil {
		t.Fatal("the stored revision was changed")
	}
}

// A revision with a deadline is never signed: the deadline is not part of the wire.
func TestSignRefusesGrantDeadlines(t *testing.T) {
	r := allGrants(t0.Add(time.Hour))
	if _, err := Sign(key(1), r); err == nil || !strings.Contains(err.Error(), "ForMamelet") {
		t.Fatalf("signed a revision with deadlines: %v", err)
	}
	if _, err := Sign(key(1), r.ForMamelet(t0)); err != nil {
		t.Fatal(err)
	}
}

// legacy is the revision as a mamelet of release 0.3.1 and before decodes it: strictly, so a member
// it does not know refuses the whole revision (the wire must not grow while a node may run one).
type legacyOnce struct {
	Operation string `json:"operation"`
	Attempt   int    `json:"attempt"`
	Action    string `json:"action"`
}

type legacyRevision struct {
	Schema    string    `json:"schema"`
	Node      string    `json:"node"`
	VMID      int       `json:"vmid"`
	Serial    int64     `json:"serial"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Packages  []struct {
		Name    string `json:"name"`
		Install *struct {
			Operation string `json:"operation"`
			Attempt   int    `json:"attempt"`
			ChangeSet string `json:"changeSet"`
		} `json:"install,omitempty"`
	} `json:"packages"`
	Services []struct {
		Name    string       `json:"name"`
		Enabled bool         `json:"enabled"`
		Running bool         `json:"running"`
		Repair  []string     `json:"repair"`
		Once    []legacyOnce `json:"once"`
	} `json:"services"`
	K3s *struct {
		Version string      `json:"version"`
		SHA256  string      `json:"sha256"`
		Role    string      `json:"role"`
		Server  string      `json:"server,omitempty"`
		Token   string      `json:"token"`
		Install *legacyOnce `json:"install,omitempty"`
		Remove  *struct {
			Node string `json:"node"`
			legacyOnce
		} `json:"remove,omitempty"`
	} `json:"k3s,omitempty"`
	Manifests []struct {
		Name    string      `json:"name"`
		Content string      `json:"content"`
		Write   *legacyOnce `json:"write,omitempty"`
	} `json:"manifests,omitempty"`
	Mamelet *struct {
		SHA256 string `json:"sha256"`
		legacyOnce
	} `json:"mamelet,omitempty"`
	ControlPlane string `json:"controlPlane,omitempty"`
}

// A Control Plane that gives its grants deadlines still sends a revision an older mamelet reads
// (the Control Plane is updated first, while the nodes still run what they ran).
func TestServedRevisionIsReadableByAnOlderMamelet(t *testing.T) {
	k := key(1)
	signed, err := Sign(k, allGrants(t0.Add(time.Hour)).ForMamelet(t0))
	if err != nil {
		t.Fatal(err)
	}
	var old legacyRevision
	if err := json.Unmarshal(signed.Payload, &old, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("a mamelet of the previous release refuses the revision: %v\n%s", err, signed.Payload)
	}
	if old.Packages[0].Install == nil || old.K3s.Remove == nil || old.Mamelet == nil || old.Manifests[0].Write == nil || len(old.Services[0].Once) != 1 {
		t.Fatalf("a grant did not arrive: %+v", old)
	}
	// The check can fail: the stored form, with the deadlines, is refused by the strict decoder.
	stored, _ := json.Marshal(allGrants(t0.Add(time.Hour)))
	if err := json.Unmarshal(stored, new(legacyRevision), json.RejectUnknownMembers(true)); err == nil {
		t.Fatal("the legacy decoder accepts deadlines: this test would not notice them on the wire")
	}
	// A mamelet of this release reads it as well.
	var self Revision
	if err := json.Unmarshal(signed.Payload, &self, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
}

// The plan tells a node's refusal apart by the words Verify gives (they are all the Control Plane
// gets: Report.Refused is text): a revision with a member the mamelet does not know starts
// "decode revision", one whose content is not valid starts "invalid revision".
func TestRefusalWordsThePlanRecognizes(t *testing.T) {
	k := key(1)
	pub := k.Public().(ed25519.PublicKey)
	sign := func(payload string) Signed {
		return Signed{Payload: []byte(payload), Signature: ed25519.Sign(k, []byte(payload))}
	}
	good, _ := json.Marshal(sample())
	unknown := strings.Replace(string(good), `"node":"web"`, `"node":"web","somethingNew":1`, 1)
	if _, err := Verify(pub, sign(unknown), expect()); err == nil || !strings.HasPrefix(err.Error(), "decode revision") {
		t.Errorf("an unknown member: %v", err)
	}
	invalid := strings.Replace(string(good), `"nginx.service"`, `"a:b"`, 1)
	if _, err := Verify(pub, sign(invalid), expect()); err == nil || !strings.HasPrefix(err.Error(), "invalid revision") {
		t.Errorf("an invalid service name: %v", err)
	}
}
