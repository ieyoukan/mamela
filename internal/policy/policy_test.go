package policy

import "testing"

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
