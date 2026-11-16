package mamelet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// How mamelet updates itself (an approved, one-time operation; the declared release is pinned by
// its sha256, like a k3s binary):
//
//  1. fetch the binary from the Control Plane (over the TLS it is pinned to), and check its sha256
//     against the approved one;
//  2. write it next to the running one (mamelet.next) and run it with -version: a binary that does
//     not run on this machine is refused, with nothing changed;
//  3. keep the running binary as mamelet.prev and put the new one in its place;
//  4. report, then restart into the new binary (the main loop does that).
//
// The new binary has to report within updateGrace, and must stay up: if it starts more than
// maxUpdateStarts times without a successful round, or does not report in time, it puts mamelet.prev
// back and restarts into it. The result of the operation says the binary was replaced; what runs
// is what the next report says (the plan updates again if it is not the declared release).
const (
	maxUpdateStarts = 3
	updateGrace     = 10 * time.Minute
)

// PendingUpdate is an update that has not proved itself yet: the new mamelet has not made one
// round that was reported.
type PendingUpdate struct {
	From      string    `json:"from"` // sha256 of the binary before
	To        string    `json:"to"`
	Operation string    `json:"operation"`
	Attempt   int       `json:"attempt"`
	Starts    int       `json:"starts"` // how many times the new binary started
	Deadline  time.Time `json:"deadline"`
}

// MameletSource is a ControlPlane that serves mamelet's binary by its sha256.
type MameletSource interface {
	Mamelet(ctx context.Context, sha256 string) ([]byte, error)
}

// SelfSHA256 is the sha256 of the mamelet binary that is running (the file /proc/self/exe is, even
// if an update has replaced the file at its path).
func (s *System) SelfSHA256() (string, error) {
	if s.selfSHA != "" {
		return s.selfSHA, nil
	}
	p := s.SelfPath
	if p == "" {
		p = "/proc/self/exe"
	}
	sum, err := fileSHA256(p)
	if err != nil {
		return "", err
	}
	s.selfSHA = sum
	return sum, nil
}

// StageMamelet checks the binary against the approved sha256, writes it as mamelet.next and runs it
// with -version. Whatever is wrong, the staged file is removed and nothing else has changed.
func (s *System) StageMamelet(ctx context.Context, data []byte, want string) error {
	sum := sha256.Sum256(data)
	switch {
	case hex.EncodeToString(sum[:]) != want:
		return fmt.Errorf("the Control Plane served a binary with sha256 %s, not the approved %s", hex.EncodeToString(sum[:]), want)
	case len(data) == 0 || len(data) > noderev.MaxMamelet:
		return fmt.Errorf("the binary is %d bytes (at most %d)", len(data), noderev.MaxMamelet)
	}
	next := s.path(noderev.MameletNextPath)
	if err := writeK3sFile(next, data, 0o755); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := s.run(cctx, next, []string{"-version"})
	if err == nil && !strings.HasPrefix(strings.TrimSpace(out), "mamelet ") {
		err = fmt.Errorf("-version printed %q", strings.TrimSpace(out))
	}
	if err != nil {
		os.Remove(next)
		return fmt.Errorf("the new mamelet does not run on this machine: %w", err)
	}
	return nil
}

// SwapMamelet keeps the running binary as mamelet.prev and puts the staged one in its place.
func (s *System) SwapMamelet() error {
	cur, prev, next := s.path(noderev.MameletPath), s.path(noderev.MameletPrevPath), s.path(noderev.MameletNextPath)
	data, err := os.ReadFile(cur)
	if err != nil {
		return err
	}
	if err := writeK3sFile(prev, data, 0o755); err != nil {
		return err
	}
	return os.Rename(next, cur)
}

// RestoreMamelet puts mamelet.prev back in place.
func (s *System) RestoreMamelet() error {
	data, err := os.ReadFile(s.path(noderev.MameletPrevPath))
	if err != nil {
		return fmt.Errorf("no previous mamelet to go back to: %w", err)
	}
	return writeK3sFile(s.path(noderev.MameletPath), data, 0o755)
}

// updateMamelet carries out an approved update: fetch, stage, swap. It reports how it went through
// record, and leaves the restart to the main loop (Restarting).
func (a *Agent) updateMamelet(ctx context.Context, u *noderev.MameletUpdate, st *State, record func(op string, attempt int, outcome, detail string)) {
	self, err := a.System.SelfSHA256()
	switch {
	case err != nil:
		record(u.Operation, u.Attempt, noderev.OutcomeFailed, "cannot tell which mamelet runs: "+err.Error())
		return
	case self == u.SHA256:
		record(u.Operation, u.Attempt, noderev.OutcomeDone, "mamelet already runs "+u.SHA256[:12])
		return
	}
	src, ok := a.CP.(MameletSource)
	if !ok {
		record(u.Operation, u.Attempt, noderev.OutcomeRefused, "this Control Plane client cannot fetch mamelet")
		return
	}
	data, err := src.Mamelet(ctx, u.SHA256)
	if err != nil {
		record(u.Operation, u.Attempt, noderev.OutcomeFailed, "fetch: "+err.Error())
		return
	}
	if err := a.System.StageMamelet(ctx, data, u.SHA256); err != nil {
		record(u.Operation, u.Attempt, noderev.OutcomeFailed, err.Error())
		return
	}
	if err := a.System.SwapMamelet(); err != nil {
		record(u.Operation, u.Attempt, noderev.OutcomeFailed, "replace mamelet: "+err.Error())
		return
	}
	st.Update = &PendingUpdate{From: self, To: u.SHA256, Operation: u.Operation, Attempt: u.Attempt, Deadline: a.Now().Add(updateGrace)}
	a.restart = true
	record(u.Operation, u.Attempt, noderev.OutcomeDone, "replaced mamelet with "+u.SHA256[:12]+"; restarting (the previous binary is kept)")
}

// Restarting says whether an update replaced the binary and mamelet should restart into it now
// (exec noderev.MameletPath).
func (a *Agent) Restarting() bool { return a.restart }

// AbortRestart is for a restart that could not be made: the previous binary is put back, the
// update is forgotten, and its operation is recorded as failed.
func (a *Agent) AbortRestart(why error) error {
	a.restart = false
	return a.rollback("could not restart into the new mamelet: " + why.Error())
}

// Boot is called once when mamelet starts. An update that has not proved itself counts this start;
// it says whether the new mamelet has failed too often or too long, and should go back (Rollback).
// A previous binary that runs after an update (a rollback, or a restart that did not happen) ends
// the update.
func (a *Agent) Boot() (rollback bool, err error) {
	st, err := a.State.Load()
	if err != nil || st.Update == nil {
		return false, err
	}
	self, err := a.System.SelfSHA256()
	if err != nil {
		return false, err
	}
	u := st.Update
	if self != u.To {
		st.Update = nil
		return false, a.State.Save(st)
	}
	u.Starts++
	if err := a.State.Save(st); err != nil {
		return false, err
	}
	return u.Starts > maxUpdateStarts || a.Now().After(u.Deadline), nil
}

// UpdateOverdue says whether a new mamelet that has not proved itself has run past its time.
func (a *Agent) UpdateOverdue() bool {
	st, err := a.State.Load()
	if err != nil || st.Update == nil {
		return false
	}
	self, err := a.System.SelfSHA256()
	return err == nil && self == st.Update.To && a.Now().After(st.Update.Deadline)
}

// Rollback puts the previous mamelet back in place and records the update's operation as failed;
// the caller restarts into it (Restarting).
func (a *Agent) Rollback(why string) error {
	if err := a.rollback(why); err != nil {
		return err
	}
	a.restart = true
	return nil
}

func (a *Agent) rollback(why string) error {
	st, err := a.State.Load()
	if err != nil {
		return err
	}
	u := st.Update
	if u == nil {
		return errors.New("no update to go back from")
	}
	st.Update = nil
	st.Attempts[attemptKey(u.Operation, u.Attempt)] = noderev.Result{Operation: u.Operation, Attempt: u.Attempt, Outcome: noderev.OutcomeFailed,
		Detail: "went back to the previous mamelet: " + why}
	if err := a.System.RestoreMamelet(); err != nil {
		return errors.Join(err, a.State.Save(st))
	}
	return a.State.Save(st)
}

// confirmUpdate ends an update that has proved itself: the new mamelet made a round and reported.
func (a *Agent) confirmUpdate(st *State) {
	if st.Update == nil {
		return
	}
	if self, err := a.System.SelfSHA256(); err == nil && self == st.Update.To {
		st.Update = nil
		a.State.Save(*st)
	}
}
