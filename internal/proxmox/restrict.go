package proxmox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// What a context lets the client send, and what this checks — and does not.
//
// Restrict limits a context to the calls an operation declares (its part of the provider
// manifest), and for calls that list Params, to those parameter names. Values pins parameter
// values on a call, and Bind pins the IDs in paths ({node}, {vmid}) and the parameters of the
// same name. A context made by RestrictOperation is bound to one approved operation: it sends no
// changing request until every ID its path uses is bound, so an operation can only change the
// object it was approved for, with the approved values. ReadOnly sends no changing request at all
// (observation).
//
// Not checked here: values that are not pinned (Mamela pins those that carry the approved
// intent: VMID, node, name, vCPUs, memory, pool, disk size, tags), the content of uploads, and
// preconditions beyond the configuration digest Proxmox itself checks. Read requests are allowed
// on any ID within the declared calls.
//
// A refusal is ErrNotSent: that one request was not sent. It says nothing about the requests the
// operation sent before it; Changed reports those, so a provider can tell "nothing happened" from
// "partly done".

// ErrNotSent marks a request the client refused before sending it. Only that request is known not
// to have happened.
var ErrNotSent = errors.New("not sent")

type restrictKey struct{}

// restriction is one limit on a context.
type restriction struct {
	what      string
	calls     []Call                // nil: any call (only the other fields limit)
	readOnly  bool                  // no changing request
	operation bool                  // changing requests need every path ID bound
	ids       map[string]string     // bound IDs ("node", "vmid")
	values    map[string]url.Values // by "METHOD path": pinned parameter values
	log       *changeLog
}

// changeLog records the changing requests of an operation that took effect or may have.
type changeLog struct {
	mu      sync.Mutex
	entries []string
}

func with(ctx context.Context, r restriction) context.Context {
	return context.WithValue(ctx, restrictKey{}, append(restrictions(ctx), r))
}

func restrictions(ctx context.Context) []restriction {
	r, _ := ctx.Value(restrictKey{}).([]restriction)
	return slices.Clip(r)
}

// Restrict returns a context in which the client sends only the given calls (and, for calls that
// list Params, only those parameters). what names the operation in errors. Restrictions add up:
// every one must allow a request.
func Restrict(ctx context.Context, what string, calls []Call) context.Context {
	return with(ctx, restriction{what: what, calls: calls})
}

// ReadOnly returns a context that sends no changing request (observation, planning).
func ReadOnly(ctx context.Context, what string) context.Context {
	return with(ctx, restriction{what: what, readOnly: true})
}

// RestrictOperation is Restrict for one approved operation: in addition, a changing request is
// sent only when every ID in its path is bound (Bind), and Changed reports what it changed.
func RestrictOperation(ctx context.Context, what string, calls []Call) context.Context {
	return with(ctx, restriction{what: what, calls: calls, operation: true, ids: map[string]string{}, log: &changeLog{}})
}

// Bind pins IDs for changing requests from now on: a path segment {name} and a parameter called
// name must equal the bound value.
func Bind(ctx context.Context, ids map[string]string) context.Context {
	return with(ctx, restriction{what: "binding", ids: maps.Clone(ids)})
}

// Values pins parameter values of a call ("PUT /nodes/{node}/qemu/{vmid}/config"): a request of
// that call that sends one of these parameters must send exactly this value.
func Values(ctx context.Context, method, path string, values url.Values) context.Context {
	return with(ctx, restriction{what: "approved values", values: map[string]url.Values{method + " " + path: values}})
}

// Changed lists the changing requests sent in the innermost operation of ctx that succeeded or
// whose outcome is unknown.
func Changed(ctx context.Context) []string {
	rs := restrictions(ctx)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].log != nil {
			rs[i].log.mu.Lock()
			defer rs[i].log.mu.Unlock()
			return slices.Clone(rs[i].log.entries)
		}
	}
	return nil
}

func record(ctx context.Context, method, path string) {
	for _, r := range restrictions(ctx) {
		if r.log != nil {
			r.log.mu.Lock()
			r.log.entries = append(r.log.entries, method+" "+path)
			r.log.mu.Unlock()
		}
	}
}

func changing(method string) bool { return method != http.MethodGet }

// pathIDs reads the placeholders of a catalog path from a request path.
func pathIDs(pattern, path string) map[string]string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	ids := map[string]string{}
	for i := range want {
		if i < len(got) && strings.HasPrefix(want[i], "{") && strings.HasSuffix(want[i], "}") {
			v, err := url.PathUnescape(got[i])
			if err != nil {
				v = got[i]
			}
			ids[strings.Trim(want[i], "{}")] = v
		}
	}
	return ids
}

// allowed checks a request against every restriction of ctx. ep is the catalog entry of the
// request, path the request path, params its form parameters (nil when it has none).
func allowed(ctx context.Context, ep Endpoint, path string, params url.Values) error {
	call := ep.Method + " " + ep.Path
	ids := pathIDs(ep.Path, path)
	for _, r := range restrictions(ctx) {
		if r.readOnly && changing(ep.Method) {
			return fmt.Errorf("proxmox: %s is a changing request, and %s sends none: %w", call, r.what, ErrNotSent)
		}
		if r.calls != nil {
			i := slices.IndexFunc(r.calls, func(c Call) bool { return c.Method == ep.Method && c.Path == ep.Path })
			if i < 0 {
				return fmt.Errorf("proxmox: %s is not among the calls declared for %s; "+
					"declare it (with its condition) where the operation's calls are listed, or do not make it: %w", call, r.what, ErrNotSent)
			}
			if declared := r.calls[i].Params; len(declared) > 0 && params != nil {
				var extra []string
				for _, name := range slices.Sorted(maps.Keys(params)) {
					if !slices.Contains(declared, name) {
						extra = append(extra, name)
					}
				}
				if len(extra) > 0 {
					return fmt.Errorf("proxmox: %s: parameter(s) %s are not declared for %s: %w", call, strings.Join(extra, ", "), r.what, ErrNotSent)
				}
			}
		}
		if !changing(ep.Method) {
			continue
		}
		if r.operation {
			for id := range ids {
				if _, ok := boundID(ctx, id); !ok {
					return fmt.Errorf("proxmox: %s changes {%s} %s, which %s has not been bound to yet: %w", call, id, ids[id], r.what, ErrNotSent)
				}
			}
		}
		for id, want := range r.ids {
			if got, ok := ids[id]; ok && got != want {
				return fmt.Errorf("proxmox: %s changes %s %s, but the operation is bound to %s %s: %w", call, id, got, id, want, ErrNotSent)
			}
			if got := params[id]; len(got) > 0 && (len(got) != 1 || got[0] != want) {
				return fmt.Errorf("proxmox: %s sends %s=%v, but the operation is bound to %s: %w", call, id, got, want, ErrNotSent)
			}
		}
		for name, want := range r.values[call] {
			if got, sent := params[name]; sent && !slices.Equal(got, want) {
				return fmt.Errorf("proxmox: %s sends %s=%v, but %s=%v was approved: %w", call, name, got, name, want, ErrNotSent)
			}
		}
	}
	return nil
}

// boundID is the value an ID is bound to in ctx (the last binding wins only if equal; any two
// different bindings make every changing request fail through the check above).
func boundID(ctx context.Context, id string) (string, bool) {
	for _, r := range restrictions(ctx) {
		if v, ok := r.ids[id]; ok {
			return v, true
		}
	}
	return "", false
}
