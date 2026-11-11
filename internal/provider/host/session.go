package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/toolchain"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Options configure a provider session.
type Options struct {
	// Wasmtime is the pinned wasmtime, for WASM providers (its sha256 is checked).
	Wasmtime string
	// Source and Version name the provider, as configured: a native provider runs only when
	// Trust trusts its executable under these.
	Source, Version string
	Trust           TrustFile
	// Catalog is the host's catalog of the provider's API. A catalog for another API is not used:
	// every call is then unclassified.
	Catalog protocol.Catalog
	// Upstream sends the allowed requests (nil: none can be sent).
	Upstream Upstream
	// GuardUpstream sends the guard's reads, with the guard's own (read-only) credentials; nil:
	// Upstream (ADR 0006).
	GuardUpstream Upstream
	// Artifacts makes the artifacts of an approved change ready (nil: none can be made).
	Artifacts ArtifactMaker
	// Limits bound the measured parameters of every change (the catalog's Measures), by measure:
	// the site's budget for one machine. Absent or 0: no limit.
	Limits map[string]int64
	// Now is the clock for permit expiry (default time.Now).
	Now            func() time.Time
	MaxMemoryBytes uint64        // per WASM linear memory (default 512 MiB)
	CallTimeout    time.Duration // per call to the provider, requests included (default 5 minutes)
	// ApplyTimeout bounds one apply (and lookup) instead: creating a machine from an image can take
	// long (default 2 hours).
	ApplyTimeout time.Duration
	// StartTimeout bounds starting the provider and its describe (wasmtime compiles the program
	// first; default 2 minutes).
	StartTimeout time.Duration
}

// Session is one running provider.
type Session struct {
	exe      Executable
	o        Options
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	msgs     chan read
	readDone chan struct{}
	quit     chan struct{} // closed when the host stops listening
	stop     sync.Once
	stderr   *bounded
	dir      string
	next     int64
	dead     error
	manifest protocol.Manifest
	used     map[string]bool
}

type read struct {
	msg protocol.Message
	err error
}

// errNotMessage: the provider wrote something that is not a protocol message.
var errNotMessage = errors.New("not a JSON-RPC 2.0 message on standard output")

// maxMessage is the longest message accepted from a provider.
const maxMessage = 16 << 20

// Start runs a provider and asks it to describe itself.
func Start(ctx context.Context, exe Executable, o Options) (*Session, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxMemoryBytes == 0 {
		o.MaxMemoryBytes = 512 << 20
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = 5 * time.Minute
	}
	if o.ApplyTimeout == 0 {
		o.ApplyTimeout = 2 * time.Hour
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = 2 * time.Minute
	}
	if err := o.Catalog.Check(); err != nil {
		return nil, err
	}
	// Run exactly what was inspected: a private copy of the inspected bytes, not the path, which
	// may have been replaced since.
	if len(exe.data) == 0 {
		return nil, &Error{Code: CodeExecutable, Message: "the executable was not inspected (use Inspect)"}
	}
	dir, err := os.MkdirTemp("", "mamela-provider-") // 0700
	if err != nil {
		return nil, err
	}
	program := filepath.Join(dir, "provider")
	if exe.Format != FormatNative {
		program += ".wasm"
	}
	if err := os.WriteFile(program, exe.data, 0o500); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	var cmd *exec.Cmd
	switch exe.Format {
	case FormatWASIp1, FormatComponent:
		if err := checkWasmtime(o.Wasmtime); err != nil {
			os.RemoveAll(dir)
			return nil, &Error{Code: CodeExecutable, Message: err.Error(), Hint: "mamela toolchain install"}
		}
		args := []string{"run", "-C", "cache=n",
			"-S", "inherit-env=n", "-S", "inherit-network=n", "-S", "tcp=n", "-S", "udp=n",
			"-S", "allow-ip-name-lookup=n", "-S", "http=n",
			"-W", "max-memory-size=" + strconv.FormatUint(o.MaxMemoryBytes, 10)}
		if exe.Format == FormatComponent {
			args = append(args, "-S", "p3=y")
		}
		cmd = exec.Command(o.Wasmtime, append(args, program)...)
		cmd.Env = []string{"LC_ALL=C"}
	case FormatNative:
		if !o.Trust.Trusts(o.Source, o.Version, exe.Digest) {
			os.RemoveAll(dir)
			return nil, &Error{Code: CodeNotTrusted,
				Message: fmt.Sprintf("%s is a native provider (%s), and no trust record matches it as source %q version %q", exe.Path, exe.Digest, o.Source, o.Version),
				Hint: "use the provider's WASM build; or, only if you trust its author and this exact build, " +
					"mamela provider trust --source <source> --version <version> <program> (a native provider runs outside any sandbox)"}
		}
		cmd = exec.Command(program)
		cmd.Env = []string{} // nothing of the user's environment; this limits mistakes, not a malicious provider
	default:
		os.RemoveAll(dir)
		return nil, &Error{Code: CodeExecutable, Message: fmt.Sprintf("%s: unknown format %q", exe.Path, exe.Format)}
	}
	cmd.Dir = dir
	s := &Session{exe: exe, o: o, cmd: cmd, dir: dir, msgs: make(chan read, 1), readDone: make(chan struct{}), quit: make(chan struct{}),
		stderr: &bounded{max: 64 << 10}, used: map[string]bool{}}
	cmd.Stderr = s.stderr
	if s.stdin, err = cmd.StdinPipe(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, &Error{Code: CodeProvider, Message: fmt.Sprintf("starting %s: %v", exe.Path, err)}
	}
	go s.readLoop(stdout)

	var d protocol.DescribeResult
	g := &gate{phase: phaseDescribe}
	if err := firstRefusal(g, s.callWithin(ctx, o.StartTimeout, g, protocol.MethodDescribe, protocol.DescribeParams{Protocol: protocol.Version}, &d)); err != nil {
		s.Close()
		return nil, err
	}
	if d.Protocol != protocol.Version {
		s.Close()
		return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf("the provider speaks %q; this Mamela speaks %q", d.Protocol, protocol.Version)}
	}
	if err := d.Manifest.Check(); err != nil {
		s.Close()
		return nil, &Error{Code: CodeProtocol, Message: "invalid manifest: " + err.Error()}
	}
	s.manifest = d.Manifest
	return s, nil
}

func checkWasmtime(path string) error {
	a, err := toolchain.Pinned().Wasmtime.Asset(toolchain.Platform())
	if err != nil {
		return err
	}
	if path == "" {
		return errors.New("no wasmtime to run a WASM provider with")
	}
	got, err := toolchain.FileSHA256(path)
	if err != nil {
		return err
	}
	if got != a.Bin {
		return fmt.Errorf("%s is not the pinned wasmtime %s", path, toolchain.Pinned().Wasmtime.Version)
	}
	return nil
}

func (s *Session) readLoop(stdout io.Reader) {
	defer close(s.readDone)
	r := bufio.NewReaderSize(stdout, 64<<10)
	for {
		var line []byte
		var err error
		for {
			var chunk []byte
			var more bool
			chunk, more, err = r.ReadLine()
			line = append(line, chunk...)
			if err != nil || !more {
				break
			}
			if len(line) > maxMessage {
				err = fmt.Errorf("a message longer than %d MiB", maxMessage>>20)
				break
			}
		}
		var rd read
		if err != nil {
			rd.err = err
		} else if err := json.Unmarshal(line, &rd.msg, json.RejectUnknownMembers(true)); err != nil || rd.msg.JSONRPC != "2.0" {
			rd.err = fmt.Errorf("%w: %.200q", errNotMessage, line)
		}
		select {
		case s.msgs <- rd:
		case <-s.quit:
			io.Copy(io.Discard, r) // let the process finish writing
			return
		}
		if rd.err != nil {
			io.Copy(io.Discard, r)
			return
		}
	}
}

// Manifest is what the provider declared.
func (s *Session) Manifest() protocol.Manifest { return s.manifest }

// SetArtifacts sets what makes the artifacts of the changes this session applies.
func (s *Session) SetArtifacts(m ArtifactMaker) { s.o.Artifacts = m }

// SetLimits sets the limits on measured parameters (Options.Limits).
func (s *Session) SetLimits(l map[string]int64) { s.o.Limits = l }

// CheckLimits checks a change's requests against the limits, before anyone approves it.
func (s *Session) CheckLimits(c protocol.Change) error {
	cat := s.catalog()
	for _, a := range c.Requests {
		if e, ok := cat.LookupApproved(a); ok {
			if err := e.CheckLimits(a.Params, s.o.Limits); err != nil {
				return err
			}
		}
	}
	return nil
}

// Executable is the provider program.
func (s *Session) Executable() Executable { return s.exe }

// Stderr is what the provider wrote to standard error (bounded).
func (s *Session) Stderr() string { return s.stderr.String() }

// catalog is the host's catalog, when it is for the provider's API.
func (s *Session) catalog() protocol.Catalog {
	if s.o.Catalog.API != "" && s.o.Catalog.API == s.manifest.API {
		return s.o.Catalog
	}
	return protocol.Catalog{}
}

// CheckPreconditions checks a change against the host's catalog: every approved request to a call
// whose catalog entry names a precondition parameter must carry it with the observed version,
// and the change must be based on that version. Without that, a change made to the object after
// approval would be overwritten.
func (s *Session) CheckPreconditions(c protocol.Change, observed string) error {
	cat := s.catalog()
	creates := false // an earlier request of the change creates the object (ADR 0004)
	for _, a := range c.Requests {
		e, ok := cat.LookupApproved(a)
		if ok && e.Effect == protocol.EffectCreate {
			creates = true
		}
		if !ok || e.Precondition == "" {
			continue
		}
		if creates && observed == "" && c.Precondition == "" {
			// A change to the object the same change creates: it did not exist when observed, so
			// there is no version to base it on and nobody else's change to lose. The host allows
			// it only after the create took effect in the same apply.
			if _, sends := a.Params[e.Precondition]; sends {
				return fmt.Errorf("%s %s sends %s, and the object it changes does not exist yet", a.Method, a.Path, e.Precondition)
			}
			continue
		}
		if observed == "" || c.Precondition != observed {
			return fmt.Errorf("%s %s needs a precondition (%s): the change must be based on the observed version, and it is based on %q (observed %q)",
				a.Method, a.Path, e.Precondition, c.Precondition, observed)
		}
		if got := a.Params[e.Precondition]; len(got) != 1 || got[0] != observed {
			return fmt.Errorf("%s %s must send %s=%s (the observed version)", a.Method, a.Path, e.Precondition, observed)
		}
	}
	return nil
}

// Verify classifies an operation's calls with the host's catalog.
func (s *Session) Verify(o protocol.Operation) protocol.Verified { return s.catalog().Verify(o) }

func (s *Session) gate(p phase, calls []protocol.Call, permit *protocol.Permit) *gate {
	g := &gate{phase: p, calls: calls, permit: permit, catalog: s.catalog(), upstream: s.o.Upstream, now: s.o.Now, artifacts: s.o.Artifacts, limits: s.o.Limits}
	if permit != nil {
		g.used = make([]bool, len(permit.Requests))
	}
	return g
}

// Observe asks the provider for the real state. It fails if the provider asked for anything but
// a declared read.
func (s *Session) Observe(ctx context.Context, resources []protocol.Resource) ([]protocol.Observed, []Request, error) {
	g := s.gate(phaseObserve, s.manifest.ObserveCalls, nil)
	var out protocol.ObserveResult
	err := s.call(ctx, g, protocol.MethodObserve, protocol.ObserveParams{Resources: resources}, &out)
	if err := firstRefusal(g, err); err != nil {
		return nil, g.record, err
	}
	for _, o := range out.Resources {
		if err := s.checkKind(o.Kind); err != nil {
			return nil, g.record, err
		}
		if o.ID == "" || !validIDs(o.IDs) {
			return nil, g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("observed %+v: an ID and valid ID names are required", o)}
		}
		// A node kind says, for each machine that exists, what Mamela needs to run mamelet on it;
		// any other kind says nothing of the sort.
		isNode := slices.ContainsFunc(s.manifest.Kinds, func(k protocol.Kind) bool { return k.Kind == o.Kind && k.Node })
		switch {
		case isNode && o.Exists && o.Owned && (o.Node == nil || o.Node.ID <= 0 || o.Node.UptimeSeconds < 0):
			return nil, g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("observed %s: a node it owns needs node.id (positive) and its state", o.ID)}
		case o.Node != nil && (o.Node.ID <= 0 || o.Node.UptimeSeconds < 0):
			return nil, g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("observed %s: node.id must be positive", o.ID)}
		case (!isNode || !o.Exists) && o.Node != nil:
			return nil, g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("observed %s: only an existing resource of a node kind has node", o.ID)}
		}
	}
	return out.Resources, g.record, nil
}

// Diff asks the provider for the changes. The provider may send no request.
func (s *Session) Diff(ctx context.Context, desired []protocol.Resource, observed []protocol.Observed) ([]protocol.Change, error) {
	g := s.gate(phaseDiff, nil, nil)
	var out protocol.DiffResult
	err := s.call(ctx, g, protocol.MethodDiff, protocol.DiffParams{Desired: desired, Observed: observed}, &out)
	if err := firstRefusal(g, err); err != nil {
		return nil, err
	}
	for _, c := range out.Changes {
		if c.Resource == "" {
			return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf("change %+v has no resource", c)}
		}
		// An explicit operation is never planned: a person asks for it (mamela act).
		if o, ok := s.manifest.Operation(c.Kind, c.Op); ok && o.Explicit {
			return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf("change of %s: %s is an explicit operation, which diff never returns", c.Resource, c.Op)}
		}
		// A difference the provider never acts on is reported as a change of an operation it
		// declares unsupported, with nothing to send: the plan shows it and denies it.
		if o, ok := s.manifest.Operation(c.Kind, c.Op); ok && o.Support == protocol.Unsupported {
			if len(c.Requests) > 0 || len(c.Artifacts) > 0 {
				return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf("change of %s: %s is unsupported and sends nothing, yet it has requests", c.Resource, c.Op)}
			}
			continue
		}
		if _, err := protocol.NewPermit(s.manifest, c, "check", "", "", "check"); err != nil {
			return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf("change of %s: %v", c.Resource, err)}
		}
	}
	return out.Changes, nil
}

// Guard runs the provider's safety check (ADR 0006): reads only, the manifest's guard calls, with
// the guard's credentials. A provider without a guard has nothing to check (ok, no findings).
func (s *Session) Guard(ctx context.Context, p protocol.GuardParams) (protocol.GuardResult, []Request, error) {
	if s.manifest.Guard == nil {
		return protocol.GuardResult{}, nil, nil
	}
	g := s.gate(phaseGuard, s.manifest.Guard.Calls, nil)
	if s.o.GuardUpstream != nil {
		g.upstream = s.o.GuardUpstream
	}
	var out protocol.GuardResult
	err := s.call(ctx, g, protocol.MethodGuard, p, &out)
	if err := firstRefusal(g, err); err != nil {
		return protocol.GuardResult{}, g.record, err
	}
	for _, f := range out.Findings {
		if f.Code == "" || f.Message == "" || !slices.Contains([]string{protocol.SeverityOK, protocol.SeverityWarn, protocol.SeverityBlock}, f.Severity) {
			return protocol.GuardResult{}, g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("guard finding %+v: a code, a severity (ok, warn, block) and a message are required", f)}
		}
	}
	return out, g.record, nil
}

// Prepare asks for the change of an explicit operation on an observed resource (ADR 0006). The
// provider may send no request. The change must be that operation, on that resource.
func (s *Session) Prepare(ctx context.Context, op protocol.Op, desired *protocol.Resource, observed protocol.Observed) (protocol.Change, error) {
	o, ok := s.manifest.Operation(observed.Kind, op)
	if !ok || !o.Explicit || o.Support != protocol.Supported {
		return protocol.Change{}, &Error{Code: CodePermit, Message: fmt.Sprintf("%s is not an explicit operation of %s %s", op, s.manifest.Provider, observed.Kind)}
	}
	g := s.gate(phasePrepare, nil, nil)
	var out protocol.PrepareResult
	err := s.call(ctx, g, protocol.MethodPrepare, protocol.PrepareParams{Op: op, Desired: desired, Observed: observed}, &out)
	if err := firstRefusal(g, err); err != nil {
		return protocol.Change{}, err
	}
	c := out.Change
	if c.Resource != observed.ID || c.Op != op || c.Kind != observed.Kind {
		return protocol.Change{}, &Error{Code: CodeProtocol, Message: fmt.Sprintf("prepare %s of %s returned %s of %s", op, observed.ID, c.Op, c.Resource)}
	}
	if !protocol.Targets(observed.IDs, c.IDs) {
		return protocol.Change{}, &Error{Code: CodeProtocol, Message: fmt.Sprintf("prepare %s of %s names objects %v, not the observed %v: an explicit operation acts on the object Mamela observed, and nothing else", op, observed.ID, c.IDs, observed.IDs)}
	}
	if _, err := protocol.NewPermit(s.manifest, c, "check", "", "", "check"); err != nil {
		return protocol.Change{}, &Error{Code: CodeProtocol, Message: fmt.Sprintf("prepare %s of %s: %v", op, observed.ID, err)}
	}
	return c, nil
}

// MaxScriptBytes bounds a script's text.
const MaxScriptBytes = 1 << 20

// Script asks the provider for one of its declared scripts. It may send no request; the text is
// for a person to review before running it.
func (s *Session) Script(ctx context.Context, p protocol.ScriptParams) (string, error) {
	if !slices.ContainsFunc(s.manifest.Scripts, func(sc protocol.Script) bool { return sc.Name == p.Name }) {
		var names []string
		for _, sc := range s.manifest.Scripts {
			names = append(names, sc.Name)
		}
		return "", &Error{Code: CodePermit, Message: fmt.Sprintf("provider %s writes no script %q (it writes: %s)", s.manifest.Provider, p.Name, orNone(names))}
	}
	g := s.gate(phaseScript, nil, nil)
	var out protocol.ScriptResult
	err := s.call(ctx, g, protocol.MethodScript, p, &out)
	if err := firstRefusal(g, err); err != nil {
		return "", err
	}
	if out.Text == "" || len(out.Text) > MaxScriptBytes || !utf8.ValidString(out.Text) {
		return "", &Error{Code: CodeProtocol, Message: fmt.Sprintf("script %s: the text must be UTF-8, 1 byte to %d bytes", p.Name, MaxScriptBytes)}
	}
	return out.Text, nil
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Outcome is what an apply did, as the host knows it from the requests it sent.
type Outcome string

const (
	OutcomeDone    Outcome = "done"        // the provider finished, and every request has a known result
	OutcomeFailed  Outcome = "failed"      // no change took effect
	OutcomePartly  Outcome = "partly-done" // some changes took effect, then it failed
	OutcomeUnknown Outcome = "unknown"     // a change may have taken effect: observe before going on
)

// Result is the outcome of one apply.
type Result struct {
	Outcome  Outcome
	Requests []Request
	Err      error // why it is not done
	// Disbelieved is true when the provider reported success and the host did not believe it:
	// a request was refused, a change did not take effect, or its outcome is unknown. A correct
	// provider returns the error instead.
	Disbelieved bool
}

// Changed lists the changing requests that took effect or may have.
func (r Result) Changed() []Request {
	var out []Request
	for _, q := range r.Requests {
		if q.Change && (q.Outcome == Done || q.Outcome == Maybe) {
			out = append(out, q)
		}
	}
	return out
}

// Apply carries out one approved change under its permit. The permit must be the one
// protocol.NewPermit derives from the change; it is used once.
func (s *Session) Apply(ctx context.Context, p protocol.Permit, c protocol.Change, desired *protocol.Resource, observed *protocol.Observed) Result {
	want, err := protocol.NewPermit(s.manifest, c, p.OperationID, p.PlanDigest, p.ProvidersDigest, p.ExpiresAt)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: &Error{Code: CodePermit, Message: err.Error()}}
	}
	if !sameJSON(want, p) {
		return Result{Outcome: OutcomeFailed, Err: &Error{Code: CodePermit, Message: "the permit is not the one derived from the change and the manifest"}}
	}
	if s.used[p.OperationID] {
		return Result{Outcome: OutcomeFailed, Err: &Error{Code: CodePermit, Message: "the permit of operation " + p.OperationID + " was already used"}}
	}
	if exp, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil || !s.o.Now().Before(exp) {
		return Result{Outcome: OutcomeFailed, Err: &Error{Code: CodePermit, Message: "the permit expired at " + p.ExpiresAt, Hint: "plan and approve again"}}
	}
	s.used[p.OperationID] = true
	g := s.gate(phaseApply, p.Calls, &p)
	var out protocol.ApplyResult
	callErr := s.callWithin(ctx, s.o.ApplyTimeout, g, protocol.MethodApply, protocol.ApplyParams{Permit: p, Change: c, Desired: desired, Observed: observed}, &out)
	return outcome(g.record, callErr)
}

// outcome decides an apply's outcome from the record, whatever the provider said.
func outcome(record []Request, callErr error) Result {
	r := Result{Requests: record}
	var done, maybe bool
	var refusal, failed *Request
	for i, q := range record {
		switch {
		case q.Outcome == Refused && refusal == nil:
			refusal = &record[i]
		case q.Change && q.Outcome == Done:
			done = true
		case q.Change && q.Outcome == Maybe:
			maybe = true
		case q.Change && (q.Outcome == Failed || q.Outcome == NotSent) && failed == nil:
			failed = &record[i]
		}
	}
	switch {
	case refusal != nil:
		e := &Error{Code: CodeRefused, Message: fmt.Sprintf("the provider asked for %s %s, which the permit does not allow: %s", refusal.Method, refusal.Path, refusal.Reason)}
		if callErr == nil {
			e.Message += "; it then reported success, which the host does not believe"
		}
		r.Err = e
	case callErr != nil:
		r.Err = callErr
	case failed != nil:
		r.Err = &Error{Code: CodeFailed, Message: fmt.Sprintf("the change request %s did not take effect (%s), and the provider reported success, which the host does not believe", failed.String(), failed.Outcome)}
	}
	r.Disbelieved = callErr == nil && (r.Err != nil || maybe)
	switch {
	case maybe:
		r.Outcome = OutcomeUnknown
		if r.Err == nil {
			r.Err = &Error{Code: CodeFailed, Message: "a change was sent and its outcome is unknown; observe before going on"}
		}
	case r.Err == nil:
		r.Outcome = OutcomeDone
	case done:
		r.Outcome = OutcomePartly
	default:
		r.Outcome = OutcomeFailed
	}
	return r
}

// Lookup asks whether a change is in effect, with reads only.
func (s *Session) Lookup(ctx context.Context, c protocol.Change) (protocol.LookupStatus, []Request, error) {
	o, ok := s.manifest.Operation(c.Kind, c.Op)
	if !ok {
		return "", nil, &Error{Code: CodePermit, Message: fmt.Sprintf("%s %s is not an operation of %s", c.Kind, c.Op, s.manifest.Provider)}
	}
	g := s.gate(phaseLookup, append(slices.Clone(s.manifest.ObserveCalls), o.Calls...), nil)
	var out protocol.LookupResult
	err := s.callWithin(ctx, s.o.ApplyTimeout, g, protocol.MethodLookup, protocol.LookupParams{Change: c}, &out)
	if err := firstRefusal(g, err); err != nil {
		return "", g.record, err
	}
	switch out.Status {
	case protocol.LookupDone, protocol.LookupNotDone, protocol.LookupPartly, protocol.LookupUnknown:
		return out.Status, g.record, nil
	}
	return "", g.record, &Error{Code: CodeProtocol, Message: fmt.Sprintf("lookup status %q", out.Status)}
}

// Close ends the provider.
func (s *Session) Close() error {
	s.stop.Do(func() { close(s.quit) })
	s.stdin.Close()
	select {
	case <-s.readDone:
	case <-time.After(5 * time.Second):
		s.cmd.Process.Kill()
		<-s.readDone
	}
	err := s.cmd.Wait()
	os.RemoveAll(s.dir)
	if s.dead != nil {
		return nil // already reported
	}
	return err
}

func (s *Session) checkKind(k protocol.ResourceKind) error {
	if !slices.ContainsFunc(s.manifest.Kinds, func(x protocol.Kind) bool { return x.Kind == k }) {
		return &Error{Code: CodeProtocol, Message: fmt.Sprintf("kind %q is not one of the provider's", k)}
	}
	return nil
}

// firstRefusal turns a refused request into the error of a read-only call: a provider that asked
// for something it may not is not believed, whatever it answered.
func firstRefusal(g *gate, err error) error {
	for _, q := range g.record {
		if q.Outcome == Refused {
			return &Error{Code: CodeRefused, Message: fmt.Sprintf("during %s the provider asked for %s %s: %s", g.phase, q.Method, q.Path, q.Reason)}
		}
	}
	return err
}

// call sends one request to the provider and serves its host/request requests until it answers.
func (s *Session) call(ctx context.Context, g *gate, method string, params, result any) error {
	return s.callWithin(ctx, s.o.CallTimeout, g, method, params, result)
}

func (s *Session) callWithin(ctx context.Context, timeout time.Duration, g *gate, method string, params, result any) error {
	if s.dead != nil {
		return s.dead
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := json.Marshal(params, json.Deterministic(true))
	if err != nil {
		return err
	}
	s.next++
	id := s.next
	if err := s.write(ctx, protocol.Message{JSONRPC: "2.0", ID: id, Method: method, Params: data}); err != nil {
		return s.fail(fmt.Sprintf("the provider did not read %s within %s", method, timeout), err)
	}
	for {
		var rd read
		select {
		case <-ctx.Done():
			return s.fail(fmt.Sprintf("the provider did not answer %s within %s", method, timeout), ctx.Err())
		case rd = <-s.msgs:
		}
		if errors.Is(rd.err, errNotMessage) {
			return s.fail("protocol", rd.err)
		}
		if rd.err != nil {
			return s.fail("the provider stopped during "+method, rd.err)
		}
		m := rd.msg
		switch {
		case m.Method == protocol.MethodHostArtifact:
			reply := protocol.Message{JSONRPC: "2.0", ID: m.ID}
			var ha protocol.HostArtifact
			if err := json.Unmarshal(m.Params, &ha, json.RejectUnknownMembers(true)); err != nil {
				reply.Error = &protocol.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
			} else if info, perr := g.artifact(ctx, ha.ID); perr != nil {
				reply.Error = perr
			} else if reply.Result, err = json.Marshal(info, json.Deterministic(true)); err != nil {
				return err
			}
			if err := s.write(ctx, reply); err != nil {
				return s.fail(fmt.Sprintf("the provider did not read the answer to its request within %s", timeout), err)
			}
		case m.Method == protocol.MethodHostRequest:
			reply := protocol.Message{JSONRPC: "2.0", ID: m.ID}
			var hr protocol.HostRequest
			if err := json.Unmarshal(m.Params, &hr, json.RejectUnknownMembers(true)); err != nil {
				reply.Error = &protocol.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
			} else if resp, perr := g.send(ctx, hr); perr != nil {
				reply.Error = perr
			} else if reply.Result, err = json.Marshal(resp, json.Deterministic(true)); err != nil {
				return err
			}
			if err := s.write(ctx, reply); err != nil {
				return s.fail(fmt.Sprintf("the provider did not read the answer to its request within %s", timeout), err)
			}
		case m.Method != "":
			return s.fail("protocol", fmt.Errorf("the provider called %q, which the host does not offer", m.Method))
		case m.ID != id:
			return s.fail("protocol", fmt.Errorf("a response to request %d while %d was pending", m.ID, id))
		case m.Error != nil:
			return &Error{Code: CodeFailed, Message: fmt.Sprintf("the provider could not %s: %s", method, m.Error.Message)}
		default:
			if err := json.Unmarshal(m.Result, result, json.RejectUnknownMembers(true)); err != nil {
				return s.fail("protocol", fmt.Errorf("the result of %s: %v", method, err))
			}
			return nil
		}
	}
}

// write sends a message. A provider that does not read its input blocks the write; the
// deadline still holds: the caller then stops the provider, which ends the write.
func (s *Session) write(ctx context.Context, m protocol.Message) error {
	b, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.stdin.Write(append(b, '\n'))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fail stops the provider: after a broken conversation nothing it says is believed.
func (s *Session) fail(what string, err error) error {
	s.stop.Do(func() { close(s.quit) })
	s.cmd.Process.Kill()
	code := CodeProvider
	if what == "protocol" {
		code, what = CodeProtocol, "the provider broke the protocol"
	}
	s.dead = &Error{Code: code, Message: fmt.Sprintf("%s: %v", what, err), Detail: s.stderr.String()}
	return s.dead
}

func validIDs(ids map[string]string) bool {
	for k, v := range ids {
		if _, err := protocol.ParsePath("/{" + k + "}"); err != nil || protocol.CheckPath("/"+v) != nil {
			return false
		}
	}
	return true
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a, json.Deterministic(true))
	y, err2 := json.Marshal(b, json.Deterministic(true))
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// bounded keeps the first max bytes written to it.
type bounded struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (b *bounded) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *bounded) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
