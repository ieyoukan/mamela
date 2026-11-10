package noderev

import (
	"crypto/ed25519"
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
