package reconcile_test

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/provider/fake"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// An apply that was killed holds its claim for the lease (hours, so that a long import is not taken
// over). The next apply does not wait for it when the worker is a process of this host that is gone,
// and does wait when the worker may be alive.
func TestClaimOfAGoneWorkerIsReleased(t *testing.T) {
	host, _ := os.Hostname()
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skipf("cannot start a process: %v", err)
	}
	for _, c := range []struct {
		name, owner string
		runs        bool
	}{
		{"a process of this host that is gone", host + "/" + strconv.Itoa(gone.Process.Pid), true},
		{"this process", host + "/" + strconv.Itoa(os.Getpid()), false},
		{"another host", "elsewhere/" + strconv.Itoa(gone.Process.Pid), false},
		{"a name that is not host/pid", "worker-b", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.plan()
			e.approve(p)
			id := "FakeVM app-server"
			opID := reconcile.OperationID(p.Digest, id, fake.OpCreate)
			if _, err := e.store.Intend(e.ctx, opID, p.Digest, id, string(fake.OpCreate)); err != nil {
				t.Fatal(err)
			}
			if ok, err := e.store.Claim(e.ctx, opID, c.owner, 3*time.Hour); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			rep := e.apply(p)
			got := outcomes(rep)[fake.OpCreate]
			if c.runs && (got != reconcile.OutcomeDone || e.world.Calls(fake.OpCreate) != 1) {
				t.Fatalf("the claim of a gone worker was not released: %s %+v", got, rep.Stopped)
			}
			if !c.runs && (got != reconcile.OutcomeBusy || e.world.Calls(fake.OpCreate) != 0 || rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0005") {
				t.Fatalf("a claim that may be alive was taken over: %s %+v", got, rep.Stopped)
			}
		})
	}
}

// A restarted service is often given the pid of the one before it, or another process takes a pid
// after a reboot: a claim named by pid alone is taken for a live worker's until its lease (hours)
// ends. The name carries the process's start, and a claim whose pid is alive but started at another
// time is a gone worker's (B8).
func TestClaimOfAReusedPidIsReleased(t *testing.T) {
	host, _ := os.Hostname()
	other := exec.Command("sleep", "60") // a live process that is not the claim's worker
	if err := other.Start(); err != nil {
		t.Skipf("cannot start a process: %v", err)
	}
	defer other.Process.Kill()
	starts := map[int]string{other.Process.Pid: "boot-2:5000", os.Getpid(): "boot-2:7000"}
	defer reconcile.SetProcessStart(func(pid int) (string, bool) { s, ok := starts[pid]; return s, ok })()

	for _, c := range []struct {
		name, owner string
		runs        bool
	}{
		{"the pid is in use by a process that started later", host + "/" + strconv.Itoa(other.Process.Pid) + "#boot-2:100", true},
		{"the pid is in use since a reboot", host + "/" + strconv.Itoa(other.Process.Pid) + "#boot-1:5000", true},
		{"the worker itself, still running", host + "/" + strconv.Itoa(other.Process.Pid) + "#boot-2:5000", false},
		{"this pid, of a process that is gone (this one was started in its place)", host + "/" + strconv.Itoa(os.Getpid()) + "#boot-2:1", true},
		{"this process", host + "/" + strconv.Itoa(os.Getpid()) + "#boot-2:7000", false},
		{"a name written before starts were recorded, pid in use", host + "/" + strconv.Itoa(other.Process.Pid), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.plan()
			e.approve(p)
			id := "FakeVM app-server"
			opID := reconcile.OperationID(p.Digest, id, fake.OpCreate)
			if _, err := e.store.Intend(e.ctx, opID, p.Digest, id, string(fake.OpCreate)); err != nil {
				t.Fatal(err)
			}
			if ok, err := e.store.Claim(e.ctx, opID, c.owner, 3*time.Hour); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			rep := e.apply(p)
			got := outcomes(rep)[fake.OpCreate]
			if c.runs && (got != reconcile.OutcomeDone || e.world.Calls(fake.OpCreate) != 1) {
				t.Fatalf("the claim of another process with the same pid was not released: %s %+v", got, rep.Stopped)
			}
			if !c.runs && (got != reconcile.OutcomeBusy || e.world.Calls(fake.OpCreate) != 0) {
				t.Fatalf("the claim of a live worker was taken over: %s %+v", got, rep.Stopped)
			}
		})
	}
}

// What a process writes as its name is its own: host, pid and start, where the start can be told.
func TestWorkerNameCarriesTheStart(t *testing.T) {
	host, _ := os.Hostname()
	defer reconcile.SetProcessStart(func(pid int) (string, bool) { return "boot-9:42", true })()
	if got, want := reconcile.WorkerName(), host+"/"+strconv.Itoa(os.Getpid())+"#boot-9:42"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	defer reconcile.SetProcessStart(func(pid int) (string, bool) { return "", false })()
	if got, want := reconcile.WorkerName(), host+"/"+strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("without a start: %q, want %q", got, want)
	}
}

// /proc/<pid>/stat: the command's name is in parentheses and can hold spaces and parentheses; the
// start is the 22nd field.
func TestStartFromStat(t *testing.T) {
	for _, c := range []struct{ name, stat, want string }{
		{"plain", "1234 (mamela) S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 8 0 987654 1000000 500 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0", "987654"},
		{"a name with spaces and parentheses", "77 (a b) (c) S 1 77 77 0 -1 4194560 1 0 0 0 1 1 0 0 20 0 1 0 555 1 1 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0", "555"},
		{"too short", "1 (x) S 1 1", ""},
		{"no parenthesis", "garbage", ""},
	} {
		got, ok := reconcile.StartFromStat([]byte(c.stat))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: %q %v, want %q", c.name, got, ok, c.want)
		}
	}
}
