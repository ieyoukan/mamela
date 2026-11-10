package proxmox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// An operation's declared calls (its part of a provider manifest) are not documentation: while
// the operation runs, the client sends nothing else. A call or parameter the implementation makes
// without declaring it fails before it is sent, so the declaration cannot fall behind the code
// without every test of that operation failing.

// ErrNotSent marks a request the client refused before sending it: its outcome is known (nothing
// happened), unlike a request that was sent and got no answer.
var ErrNotSent = errors.New("not sent")

type restrictKey struct{}

// restriction is the set of calls allowed in a context.
type restriction struct {
	what  string
	calls []Call
}

// Restrict returns a context in which the client sends only the given calls (and, for calls that
// list Params, only those parameters). what names the operation in errors. Restricting an
// already restricted context narrows it: both must allow a call.
func Restrict(ctx context.Context, what string, calls []Call) context.Context {
	return context.WithValue(ctx, restrictKey{}, append(restrictions(ctx), restriction{what: what, calls: calls}))
}

func restrictions(ctx context.Context) []restriction {
	r, _ := ctx.Value(restrictKey{}).([]restriction)
	return slices.Clip(r)
}

// allowed checks a request against every restriction of ctx. ep is the catalog entry of the
// request; params are its form parameters (nil when it has none).
func allowed(ctx context.Context, ep Endpoint, params url.Values) error {
	for _, r := range restrictions(ctx) {
		i := slices.IndexFunc(r.calls, func(c Call) bool { return c.Method == ep.Method && c.Path == ep.Path })
		if i < 0 {
			return fmt.Errorf("proxmox: %s %s is not among the calls declared for %s; "+
				"declare it (with its condition) where the operation's calls are listed, or do not make it: %w", ep.Method, ep.Path, r.what, ErrNotSent)
		}
		declared := r.calls[i].Params
		if len(declared) == 0 || params == nil {
			continue
		}
		var extra []string
		for _, name := range slices.Sorted(maps.Keys(params)) {
			if !slices.Contains(declared, name) {
				extra = append(extra, name)
			}
		}
		if len(extra) > 0 {
			return fmt.Errorf("proxmox: %s %s: parameter(s) %s are not declared for %s: %w", ep.Method, ep.Path, strings.Join(extra, ", "), r.what, ErrNotSent)
		}
	}
	return nil
}
