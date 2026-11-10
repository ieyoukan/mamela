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
}

// send checks a request, sends it if allowed, records it and returns the answer for the provider.
func (g *gate) send(ctx context.Context, r protocol.HostRequest) (protocol.HostResponse, *protocol.Error) {
	rec := Request{Method: r.Method, Path: r.Path, Change: true}
	reason := g.check(r, &rec)
	if reason != "" {
		rec.Outcome, rec.Reason = Refused, reason
		g.record = append(g.record, rec)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeRefused, Message: string(CodeRefused) + ": " + reason}
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
		g.record = append(g.record, rec)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeNotSent, Message: err.Error()}
	case err != nil:
		rec.Outcome, rec.Reason = Maybe, err.Error()
		if !rec.Change {
			rec.Outcome = Failed
		}
		g.record = append(g.record, rec)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeUncertain, Message: err.Error()}
	case !rec.Change || status >= 200 && status < 300:
		rec.Outcome = Done
	case status >= 500:
		rec.Outcome = Maybe
	default:
		rec.Outcome = Failed
	}
	g.record = append(g.record, rec)
	return protocol.HostResponse{Status: status, Body: string(body)}, nil
}

// check returns why the request is not allowed, or "".
func (g *gate) check(r protocol.HostRequest, rec *Request) string {
	switch g.phase {
	case phaseDescribe, phaseDiff:
		return fmt.Sprintf("no request is allowed during %s (diff computes from its parameters alone)", g.phase)
	}
	if !slices.Contains(protocol.Methods, r.Method) {
		return fmt.Sprintf("method %q", r.Method)
	}
	if err := protocol.CheckPath(r.Path); err != nil {
		return err.Error()
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
		return fmt.Sprintf("%s %s is not a declared call of this %s", r.Method, r.Path, g.what())
	}
	rec.Call = call.Method + " " + call.Path
	params := map[string][]string{}
	for k, v := range r.Query {
		params[k] = append(params[k], v...)
	}
	for k, v := range r.Form {
		params[k] = append(params[k], v...)
	}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if !slices.Contains(call.Params, k) {
			return fmt.Sprintf("parameter %q is not declared for %s", k, rec.Call)
		}
	}
	entry, classified := g.catalog.Lookup(call.Method, call.Path)
	if classified {
		rec.Effect, rec.Change = entry.Effect, entry.Effect != protocol.EffectRead
	}
	switch g.phase {
	case phaseObserve, phaseLookup:
		if rec.Change {
			what := "unclassified (the host's catalog does not know it)"
			if classified {
				what = string(entry.Effect)
			}
			return fmt.Sprintf("only reads are allowed during %s; %s is %s", g.phase, rec.Call, what)
		}
		return ""
	case phaseApply:
		return g.checkPermit(r, rec, call, entry, ids, params)
	}
	return "unknown phase"
}

func (g *gate) what() string {
	if g.permit != nil {
		return "operation (" + string(g.permit.Op) + ")"
	}
	return string(g.phase)
}

func (g *gate) checkPermit(r protocol.HostRequest, rec *Request, call protocol.Call, entry protocol.Entry, ids map[string]string, params map[string][]string) string {
	p := g.permit
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil || !g.now().Before(exp) {
		return "the permit expired at " + p.ExpiresAt
	}
	for _, pin := range p.Values {
		if pin.Method != call.Method || pin.Path != call.Path {
			continue
		}
		for _, v := range params[pin.Param] {
			if !slices.Contains(pin.Values, v) {
				return fmt.Sprintf("%s=%q is not an approved value (approved: %s)", pin.Param, v, strings.Join(pin.Values, ", "))
			}
		}
	}
	if !rec.Change {
		return "" // reads are not bound to the operation's objects
	}
	for _, name := range slices.Sorted(maps.Keys(ids)) {
		want, ok := p.IDs[name]
		if !ok {
			return fmt.Sprintf("%s is not bound by the permit; a change must name only the approved objects", name)
		}
		if ids[name] != want {
			return fmt.Sprintf("%s %s is not the approved %s", name, ids[name], want)
		}
	}
	for name, want := range p.IDs {
		for _, v := range params[name] {
			if v != want {
				return fmt.Sprintf("parameter %s=%s is not the approved %s", name, v, want)
			}
		}
	}
	if p.Precondition != "" && entry.Precondition != "" {
		if got := params[entry.Precondition]; len(got) != 1 || got[0] != p.Precondition {
			return fmt.Sprintf("%s must be the observed version %s (the change is based on it)", entry.Precondition, p.Precondition)
		}
	}
	return ""
}
