package policy

import (
	"strings"
	"testing"
)

func TestDecisions(t *testing.T) {
	p := Default
	p.AutoStartServices = []string{"nginx"}
	tests := []struct {
		s    Subject
		want Verdict
	}{
		{Subject{Op: OpInstallPackage}, ApprovalRequired},
		{Subject{Op: OpEnableService, Service: "nginx"}, ApprovalRequired},
		{Subject{Op: OpStartService, Service: "nginx"}, Auto},
		{Subject{Op: OpStartService, Service: "postgresql"}, ApprovalRequired},
		{Subject{Op: OpStopService, Service: "nginx"}, ApprovalRequired},
		{Subject{Op: OpDisableService, Service: "nginx"}, ApprovalRequired},
		{Subject{Op: OpChangeForeign}, Denied},
		{Subject{Op: "delete-vm"}, Denied},
		{Subject{Op: "run-shell"}, Denied},
		{Subject{Op: ""}, Denied},
	}
	for _, tt := range tests {
		d := p.Decide(tt.s)
		if d.Verdict != tt.want {
			t.Errorf("%s(%s) = %s, want %s", tt.s.Op, tt.s.Service, d.Verdict, tt.want)
		}
		if d.Code == "" || d.Message == "" {
			t.Errorf("%s: decision without code or message: %+v", tt.s.Op, d)
		}
	}
}

func TestDefaultStartsNothingAutomatically(t *testing.T) {
	if d := Default.Decide(Subject{Op: OpStartService, Service: "nginx"}); d.Verdict != ApprovalRequired {
		t.Errorf("default policy: %s", d.Verdict)
	}
}

func TestStricter(t *testing.T) {
	if Stricter(Auto, ApprovalRequired) != ApprovalRequired || Stricter(Denied, Auto) != Denied || Stricter(Auto, Auto) != Auto {
		t.Error("Stricter is wrong")
	}
}

func TestDigestIgnoresListOrder(t *testing.T) {
	a, b := Default, Default
	a.AutoStartServices = []string{"nginx", "cron"}
	b.AutoStartServices = []string{"cron", "nginx"}
	if a.Digest() != b.Digest() {
		t.Error("service order changed the policy digest")
	}
	if a.Digest() == Default.Digest() {
		t.Error("different policies share a digest")
	}
}

// Observe-only names (MAM-POL-0312) hold every change of them and on them; the Control Plane's own
// host holds first (MAM-POL-0308). The names are part of the digest, in any order, once each.
func TestObserveOnly(t *testing.T) {
	p := Default
	p.ObserveOnly = []string{"web", "legacy"}
	p.ProtectedNodes = []string{"cp", "web"}
	for name, want := range map[string]Code{"web": "MAM-POL-0308", "cp": "MAM-POL-0308", "legacy": "MAM-POL-0312", "db": "", "": ""} {
		d, ok := p.Held(name)
		if ok != (want != "") || d.Code != want || ok && (d.Verdict != Denied || d.Message == "" || d.Hint == "") || p.Holds(name) != ok {
			t.Errorf("Held(%q) = %+v %v, want %q", name, d, ok, want)
		}
	}
	if !p.ObservesOnly("legacy") || p.ObservesOnly("cp") || p.Protected("legacy") {
		t.Error("ObservesOnly or Protected")
	}
	q := p
	q.ObserveOnly = []string{"legacy", "web", "legacy"}
	if p.Digest() != q.Digest() {
		t.Error("the order of observeOnly, or a name twice, changed the digest")
	}
	if p.Digest() == (Policy{Version: p.Version, MaxObservationAgeSeconds: p.MaxObservationAgeSeconds, AllowedAptOrigins: p.AllowedAptOrigins,
		ProtectedNodes: p.ProtectedNodes}).Digest() {
		t.Error("observeOnly is not part of the digest")
	}
	// No observeOnly: the digest of a policy is what it was before observeOnly existed.
	if !strings.Contains(string(Default.Canonical()), `"allowedAptOrigins"`) || strings.Contains(string(Default.Canonical()), "observeOnly") {
		t.Errorf("%s", Default.Canonical())
	}
}

func TestDecodeObserveOnly(t *testing.T) {
	p, err := Decode([]byte(`{"version":"x","maxObservationAgeSeconds":600,"allowedAptOrigins":["Ubuntu:"],"observeOnly":["web","ProxmoxVM:a"]}`))
	if err != nil || !p.ObservesOnly("web") {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{`["../etc"]`, `[""]`, `["a b"]`, `["-web"]`, `"web"`} {
		if _, err := Decode([]byte(`{"version":"x","maxObservationAgeSeconds":600,"observeOnly":` + bad + `}`)); err == nil {
			t.Errorf("observeOnly %s: accepted", bad)
		}
	}
}
