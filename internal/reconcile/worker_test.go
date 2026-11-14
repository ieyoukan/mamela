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
