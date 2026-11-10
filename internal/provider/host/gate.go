package host

import (
	"context"
	"errors"
	"fmt"
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
}

// send checks a request, sends it if allowed, records it and returns the answer for the provider.
func (g *gate) send(ctx context.Context, r protocol.HostRequest) (protocol.HostResponse, *protocol.Error) {
	rec := Request{Method: r.Method, Path: r.Path, Change: true}
	approved, reason := g.check(r, &rec)
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
		rec.Outcome, rec.Reason = NotSent, "no API is configured for this provider"
		g.record = append(g.record, rec)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeNotSent, Message: rec.Reason}
	}
	status, body, err := g.upstream.Do(ctx, r)
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
	return protocol.HostResponse{Status: status, Body: string(body)}, nil
}

// check returns why the request is not allowed ("" when it is), and the index of the approved
// request it uses (-1 for none).
func (g *gate) check(r protocol.HostRequest, rec *Request) (int, string) {
	switch g.phase {
	case phaseDescribe, phaseDiff:
		return -1, fmt.Sprintf("no request is allowed during %s (diff computes from its parameters alone)", g.phase)
	}
	if !slices.Contains(protocol.Methods, r.Method) {
		return -1, fmt.Sprintf("method %q", r.Method)
	}
	if err := protocol.CheckPath(r.Path); err != nil {
		return -1, err.Error()
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
		return -1, fmt.Sprintf("%s %s is not a declared call of this %s", r.Method, r.Path, g.what())
	}
	rec.Call = call.Method + " " + call.Path
	entry, classified := g.catalog.Lookup(call.Method, call.Path)
	if classified {
		rec.Effect, rec.Change = entry.Effect, entry.Effect != protocol.EffectRead
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
				return -1, fmt.Sprintf("parameter %q is sent both in the query and in the form", k)
			}
			if call.In(k) != part.in {
				return -1, fmt.Sprintf("parameter %q is not declared in the %s of %s", k, part.in, rec.Call)
			}
			if classified && entry.In(k) != part.in {
				return -1, fmt.Sprintf("parameter %q: the API does not read it from the %s of %s", k, part.in, rec.Call)
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
			return -1, fmt.Sprintf("only reads are allowed during %s; %s is %s", g.phase, rec.Call, what)
		}
		return -1, ""
	case phaseApply:
		return g.checkPermit(rec, call, entry, ids, params)
	}
	return -1, "unknown phase"
}

func (g *gate) what() string {
	if g.permit != nil {
		return "operation (" + string(g.permit.Op) + ")"
	}
	return string(g.phase)
}

func (g *gate) checkPermit(rec *Request, call protocol.Call, entry protocol.Entry, ids map[string]string, params map[string][]string) (int, string) {
	p := g.permit
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil || !g.now().Before(exp) {
		return -1, "the permit expired at " + p.ExpiresAt
	}
	if !rec.Change {
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
	if p.Precondition != "" && entry.Precondition != "" {
		if got := params[entry.Precondition]; len(got) != 1 || got[0] != p.Precondition {
			return -1, fmt.Sprintf("%s must be the observed version %s (the change is based on it)", entry.Precondition, p.Precondition)
		}
	}
	// The request must be one of the approved ones, with exactly its parameters and values.
	var near []string
	for i, a := range p.Requests {
		if a.Method != call.Method || a.Path != call.Path {
			continue
		}
		if g.used[i] {
			near = append(near, "already sent once")
			continue
		}
		if sameParams(a.Params, params) {
			return i, ""
		}
		near = append(near, "approved "+formatParams(a.Params))
	}
	if len(near) == 0 {
		return -1, fmt.Sprintf("%s is not an approved request of this change", rec.Call)
	}
	return -1, fmt.Sprintf("%s with %s is not the approved request (%s)", rec.Call, formatParams(params), strings.Join(near, "; "))
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
