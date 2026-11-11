package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// phase is what the host is asking the provider to do; it decides which requests are allowed.
type phase string

const (
	phaseDescribe phase = "describe"
	phaseObserve  phase = "observe"
	phaseDiff     phase = "diff"
	phaseApply    phase = "apply"
	phaseLookup   phase = "lookup"
)

// RequestOutcome is what became of one request the provider asked for.
type RequestOutcome string

const (
	Refused RequestOutcome = "refused"  // not sent: outside what is allowed
	NotSent RequestOutcome = "not-sent" // not sent: the host could not send it
	Done    RequestOutcome = "done"     // sent; a change answered 2xx, or a read
	Failed  RequestOutcome = "failed"   // sent; a change answered with a status that says it did nothing (not 2xx, not 5xx)
	Maybe   RequestOutcome = "maybe"    // sent; a change whose outcome is unknown (no response, or 5xx)
)

// Request is the record of one request the provider asked for.
type Request struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Call    string          `json:"call,omitempty"`   // the declared call it matched: "PUT /items/{item}"
	Effect  protocol.Effect `json:"effect,omitempty"` // from the catalog; empty when unclassified
	Change  bool            `json:"change"`           // anything not classified as a read
	Outcome RequestOutcome  `json:"outcome"`
	Status  int             `json:"status,omitempty"`
	Reason  string          `json:"reason,omitempty"` // why it was refused or not sent
}

func (r Request) String() string {
	s := r.Method + " " + r.Path + ": " + string(r.Outcome)
	if r.Status != 0 {
		s += fmt.Sprintf(" (%d)", r.Status)
	}
	if r.Reason != "" {
		s += " (" + r.Reason + ")"
	}
	return s
}

// gate decides and records the requests of one call to the provider.
type gate struct {
	phase    phase
	calls    []protocol.Call
	permit   *protocol.Permit
	catalog  protocol.Catalog
	upstream Upstream
	now      func() time.Time
	record   []Request
	// used marks the permit's approved requests already sent (each is sent at most once).
	used []bool
	// halt, once set, refuses every further change: after a change whose outcome is unknown,
	// and after a refusal, nothing more changes until the world is observed again.
	halt string
	// artifacts makes the permit's artifacts ready; ready are those made in this apply.
	artifacts ArtifactMaker
	ready     map[string]Prepared
	// limits bound the measured parameters of changes (Options.Limits).
	limits map[string]int64
	// created is set once a create request of this apply took effect: the object it made did not
	// exist when the change was planned, so a change to it needs no observed version (ADR 0004).
	created bool
}

// Prepared is an artifact made ready for one apply.
type Prepared struct {
	Info protocol.ArtifactInfo
	Open func() (io.ReadCloser, error)
}

// ArtifactMaker makes an artifact ready: downloads and checks it, or makes a node's seed.
type ArtifactMaker interface {
	Make(ctx context.Context, a protocol.Artifact) (Prepared, error)
}

// artifact makes one of the permit's artifacts ready (once per apply) and returns what the
// provider may know of it.
func (g *gate) artifact(ctx context.Context, id string) (protocol.ArtifactInfo, *protocol.Error) {
	if g.phase != phaseApply || g.permit == nil {
		return protocol.ArtifactInfo{}, &protocol.Error{Code: protocol.CodeRefused, Message: string(CodeRefused) + ": artifacts are made only during apply"}
	}
	if p, ok := g.ready[id]; ok {
		return p.Info, nil
	}
	i := slices.IndexFunc(g.permit.Artifacts, func(a protocol.Artifact) bool { return a.ID == id })
	if i < 0 {
		return protocol.ArtifactInfo{}, &protocol.Error{Code: protocol.CodeRefused, Message: string(CodeRefused) + ": " + id + " is not an artifact of the permit"}
	}
	if g.artifacts == nil {
		return protocol.ArtifactInfo{}, &protocol.Error{Code: protocol.CodeNotSent, Message: "this Mamela cannot make artifacts (no artifact maker is configured)"}
	}
	p, err := g.artifacts.Make(ctx, g.permit.Artifacts[i])
	if err != nil {
		return protocol.ArtifactInfo{}, &protocol.Error{Code: protocol.CodeFailed, Message: fmt.Sprintf("artifact %s: %v", id, err)}
	}
	p.Info.ID = id
	if g.ready == nil {
		g.ready = map[string]Prepared{}
	}
	g.ready[id] = p
	return p.Info, nil
}

// send checks a request, sends it if allowed, records it and returns the answer for the provider.
func (g *gate) send(ctx context.Context, r protocol.HostRequest) (protocol.HostResponse, *protocol.Error) {
	rec := Request{Method: r.Method, Path: r.Path, Change: true}
	approved, call, reason := g.check(r, &rec)
	if reason != "" {
		rec.Outcome, rec.Reason = Refused, reason
		g.record = append(g.record, rec)
		if g.halt == "" {
			g.halt = "an earlier request was refused (" + reason + ")"
		}
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeRefused, Message: string(CodeRefused) + ": " + reason}
	}
	if approved >= 0 {
		g.used[approved] = true
	}
	if g.upstream == nil {
		rec.Outcome, rec.Reason = NotSent, "no API is configured for this provider (its section in mamela.providers.toml)"
		g.record = append(g.record, rec)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeNotSent, Message: rec.Reason}
	}
	var status int
	var body []byte
	var err error
	var statusText string
	ctx = context.WithValue(ctx, reasonSink{}, &statusText)
	if r.Upload != "" {
		up, ok := g.upstream.(Uploader)
		p := g.ready[r.Upload]
		if !ok {
			err = fmt.Errorf("%w: this API connection cannot upload files", ErrNotSent)
		} else {
			status, body, err = up.Upload(ctx, r, File{Field: call.File, Order: call.Form, Name: p.Info.Name, Size: p.Info.Size, Open: p.Open})
		}
	} else {
		status, body, err = g.upstream.Do(ctx, r)
	}
	rec.Status = status
	switch {
	case err != nil && errors.Is(err, ErrNotSent):
		rec.Outcome, rec.Reason = NotSent, err.Error()
	case err != nil:
		rec.Outcome, rec.Reason = Maybe, err.Error()
		if !rec.Change {
			rec.Outcome = Failed
		}
	case !rec.Change || status >= 200 && status < 300:
		rec.Outcome = Done
	case status >= 500:
		rec.Outcome = Maybe
	default:
		rec.Outcome = Failed
	}
	if rec.Outcome == Done && rec.Effect == protocol.EffectCreate {
		g.created = true
	}
	if rec.Outcome == Maybe && g.halt == "" {
		g.halt = fmt.Sprintf("the outcome of %s %s is unknown; nothing more changes until the world is observed again", r.Method, r.Path)
	}
	g.record = append(g.record, rec)
	switch {
	case err != nil && errors.Is(err, ErrNotSent):
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeNotSent, Message: err.Error()}
	case err != nil:
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeUncertain, Message: err.Error()}
	}
	return protocol.HostResponse{Status: status, Body: string(body), Reason: statusText}, nil
}

// check returns the index of the approved request a request uses (-1 for none), the declared call
// it matched, and why it is not allowed ("" when it is).
func (g *gate) check(r protocol.HostRequest, rec *Request) (int, protocol.Call, string) {
	switch g.phase {
	case phaseDescribe, phaseDiff:
		return -1, protocol.Call{}, fmt.Sprintf("no request is allowed during %s (diff computes from its parameters alone)", g.phase)
	}
	if !slices.Contains(protocol.Methods, r.Method) {
		return -1, protocol.Call{}, fmt.Sprintf("method %q", r.Method)
	}
	if err := protocol.CheckPath(r.Path); err != nil {
		return -1, protocol.Call{}, err.Error()
	}
	var call protocol.Call
	var ids map[string]string
	found := false
	for _, c := range g.calls {
		p, err := protocol.ParsePath(c.Path)
		if err != nil || c.Method != r.Method {
			continue
		}
		if m, ok := p.Match(r.Path); ok {
			call, ids, found = c, m, true
			break
		}
	}
	if !found {
		return -1, protocol.Call{}, fmt.Sprintf("%s %s is not a declared call of this %s", r.Method, r.Path, g.what())
	}
	rec.Call = call.Method + " " + call.Path
	entry, classified := g.catalog.Lookup(call.Method, call.Path)
	if classified {
		rec.Effect, rec.Change = entry.Effect, entry.Effect != protocol.EffectRead
	}
	// An upload sends its artifact as the file parameter the call declares and, for a call the
	// catalog knows, the one the API reads; the provider never sends the file itself.
	if (r.Upload != "") != (call.File != "") {
		return -1, protocol.Call{}, fmt.Sprintf("%s: an upload names an artifact, and only an upload call sends one", rec.Call)
	}
	if call.File != "" && classified && entry.File != call.File {
		return -1, protocol.Call{}, fmt.Sprintf("%s: the API reads the file from %q, not %q", rec.Call, entry.File, call.File)
	}
	// Each parameter is sent where the call declares it and, for a call the catalog knows, where
	// the API reads it: what the host checks is what the API acts on.
	params := map[string][]string{}
	for _, part := range []struct {
		in   string
		vals map[string][]string
	}{{"query", r.Query}, {"form", r.Form}} {
		for _, k := range slices.Sorted(maps.Keys(part.vals)) {
			if _, twice := params[k]; twice {
				return -1, protocol.Call{}, fmt.Sprintf("parameter %q is sent both in the query and in the form", k)
			}
			if call.In(k) != part.in {
				return -1, protocol.Call{}, fmt.Sprintf("parameter %q is not declared in the %s of %s", k, part.in, rec.Call)
			}
			if classified && entry.In(k) != part.in {
				return -1, protocol.Call{}, fmt.Sprintf("parameter %q: the API does not read it from the %s of %s", k, part.in, rec.Call)
			}
			params[k] = part.vals[k]
		}
	}
	switch g.phase {
	case phaseObserve, phaseLookup:
		if rec.Change {
			what := "unclassified (the host's catalog does not know it)"
			if classified {
				what = string(entry.Effect)
			}
			return -1, protocol.Call{}, fmt.Sprintf("only reads are allowed during %s; %s is %s", g.phase, rec.Call, what)
		}
		return -1, protocol.Call{}, ""
	case phaseApply:
		i, reason := g.checkPermit(rec, call, entry, ids, params, r.Upload)
		return i, call, reason
	}
	return -1, protocol.Call{}, "unknown phase"
}

func (g *gate) what() string {
	if g.permit != nil {
		return "operation (" + string(g.permit.Op) + ")"
	}
	return string(g.phase)
}

func (g *gate) checkPermit(rec *Request, call protocol.Call, entry protocol.Entry, ids map[string]string, params map[string][]string, upload string) (int, string) {
	p := g.permit
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil || !g.now().Before(exp) {
		return -1, "the permit expired at " + p.ExpiresAt
	}
	if !rec.Change {
		if upload != "" {
			return -1, "an upload is a change"
		}
		return -1, "" // reads are not bound to the operation's objects
	}
	if g.halt != "" {
		return -1, g.halt
	}
	for _, name := range slices.Sorted(maps.Keys(ids)) {
		want, ok := p.IDs[name]
		if !ok {
			return -1, fmt.Sprintf("%s is not bound by the permit; a change must name only the approved objects", name)
		}
		if ids[name] != want {
			return -1, fmt.Sprintf("%s %s is not the approved %s", name, ids[name], want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.IDs)) {
		for _, v := range params[name] {
			if v != p.IDs[name] {
				return -1, fmt.Sprintf("parameter %s=%s is not the approved %s", name, v, p.IDs[name])
			}
		}
	}
	// A call the catalog gives a precondition always carries the observed version: without it, a
	// change made after approval would be overwritten. The one exception is an object this apply
	// created (the change was planned when it did not exist): nobody else's change can be lost.
	if entry.Precondition != "" {
		switch {
		case p.Precondition == "" && !g.created:
			return -1, fmt.Sprintf("%s needs a precondition (%s), and the change is based on no observed version", rec.Call, entry.Precondition)
		case p.Precondition != "":
			if got := params[entry.Precondition]; len(got) != 1 || got[0] != p.Precondition {
				return -1, fmt.Sprintf("%s must be the observed version %s (the change is based on it)", entry.Precondition, p.Precondition)
			}
		}
	}
	// What it makes or changes stays within the site's budget for one machine, whatever was
	// approved: the catalog says which parameters size it.
	if err := entry.CheckLimits(params, g.limits); err != nil {
		return -1, err.Error()
	}
	if upload != "" {
		if _, ok := g.ready[upload]; !ok {
			return -1, fmt.Sprintf("artifact %s is not ready (ask host/artifact first)", upload)
		}
	}
	ready := map[string]protocol.ArtifactInfo{}
	for id, a := range g.ready {
		ready[id] = a.Info
	}
	// The request must be one of the approved ones, with exactly its parameters and values (with
	// the facts of the ready artifacts filled in), and the same upload.
	var near []string
	for i, a := range p.Requests {
		if a.Method != call.Method || a.Path != call.Path {
			continue
		}
		if g.used[i] {
			near = append(near, "already sent once")
			continue
		}
		want, err := protocol.FillArtifacts(a.Params, ready)
		if err != nil {
			near = append(near, err.Error())
			continue
		}
		if a.Upload == upload && sameParams(want, params) {
			return i, ""
		}
		near = append(near, "approved "+formatParams(want)+uploadText(a.Upload))
	}
	if len(near) == 0 {
		return -1, fmt.Sprintf("%s is not an approved request of this change", rec.Call)
	}
	return -1, fmt.Sprintf("%s with %s%s is not the approved request (%s)", rec.Call, formatParams(params), uploadText(upload), strings.Join(near, "; "))
}

func uploadText(id string) string {
	if id == "" {
		return ""
	}
	return " and upload " + id
}

// sameParams: the same names, each with the same values in any order.
func sameParams(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !slices.Equal(slices.Sorted(slices.Values(av)), slices.Sorted(slices.Values(bv))) {
			return false
		}
	}
	return true
}

func formatParams(ps map[string][]string) string {
	if len(ps) == 0 {
		return "no parameters"
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(ps)) {
		out = append(out, k+"="+strings.Join(ps[k], ","))
	}
	return strings.Join(out, " ")
}
