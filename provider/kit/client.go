package kit

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Client sends requests to the API through the Mamela host: it places each parameter where the
// declared call says the API reads it (query, form, header, JSON body), and fills a path's
// placeholders from the change's IDs.
type Client struct {
	ctx   context.Context
	h     *sdk.Host
	calls []protocol.Call
	ready map[string]protocol.ArtifactInfo
	parse func(status int, reason, body string) error
	// Claimed are the IDs other plans are to create (ADR 0007's claims): a kind that chooses IDs
	// (Missing) treats them as taken.
	Claimed []map[string]string
}

// Context is the call's context.
func (c *Client) Context() context.Context { return c.ctx }

// Host is the host, for what Client does not cover.
func (c *Client) Host() *sdk.Host { return c.h }

// APIError is an API's answer that is not a success.
type APIError struct {
	Status       int
	Reason, Body string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("status %d %s: %.300s", e.Status, e.Reason, e.Body)
}

// NotFound says whether an error is an API's "not found" (404).
func NotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == 404
}

// Do sends a request as it is, and returns the body of a success (2xx).
func (c *Client) Do(r protocol.HostRequest) ([]byte, error) {
	resp, err := c.h.Request(c.ctx, r)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		if c.parse != nil {
			return nil, c.parse(resp.Status, resp.Reason, resp.Body)
		}
		return nil, &APIError{Status: resp.Status, Reason: resp.Reason, Body: resp.Body}
	}
	return []byte(resp.Body), nil
}

var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

// Send sends an approved request of a change: its path's placeholders are the change's IDs, the
// artifacts' facts are filled in ({artifact:seed.name}), and each parameter goes where its call
// declares it.
func (c *Client) Send(ch protocol.Change, a protocol.Approved) ([]byte, error) {
	params, err := protocol.FillArtifacts(a.Params, c.ready)
	if err != nil {
		return nil, err
	}
	r, err := c.request(a.API, a.Method, a.Path, ch.IDs, params)
	if err != nil {
		return nil, err
	}
	r.Upload = a.Upload
	return c.Do(r)
}

// Read sends a read: the call's method and path, the placeholders filled from ids, the params
// placed as the call declares them.
func (c *Client) Read(call protocol.Call, ids map[string]string, params map[string][]string) ([]byte, error) {
	all := maps.Clone(params)
	if all == nil {
		all = map[string][]string{}
	}
	for k, v := range call.Fixed {
		all[k] = []string{v}
	}
	r, err := c.request(call.API, call.Method, call.Path, ids, all)
	if err != nil {
		return nil, err
	}
	return c.Do(r)
}

// ReadJSON sends a read and decodes its JSON answer into out.
func (c *Client) ReadJSON(call protocol.Call, ids map[string]string, params map[string][]string, out any) error {
	body, err := c.Read(call, ids, params)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (c *Client) request(api, method, path string, ids map[string]string, params map[string][]string) (protocol.HostRequest, error) {
	call, _, ok := protocol.FindCall(c.calls, api, method, path, params, false)
	if !ok {
		return protocol.HostRequest{}, fmt.Errorf("%s %s is not one declared call of the provider", method, path)
	}
	var missing []string
	concrete := placeholder.ReplaceAllStringFunc(path, func(m string) string {
		v, ok := ids[m[1:len(m)-1]]
		if !ok {
			missing = append(missing, m)
		}
		return v
	})
	if len(missing) > 0 {
		return protocol.HostRequest{}, fmt.Errorf("%s %s: the change's IDs give no %s", method, path, strings.Join(missing, ", "))
	}
	r := protocol.HostRequest{API: api, Method: method, Path: concrete}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		v := params[k]
		switch call.In(k) {
		case "query":
			if r.Query == nil {
				r.Query = map[string][]string{}
			}
			r.Query[k] = v
		case "header":
			if r.Header == nil {
				r.Header = map[string][]string{}
			}
			r.Header[k] = v
		case "json":
			if r.JSON == nil {
				r.JSON = map[string]jsontext.Value{}
			}
			if len(v) != 1 {
				return r, fmt.Errorf("JSON member %s has %d values", k, len(v))
			}
			r.JSON[k] = jsontext.Value(v[0])
		default: // form (and what the call does not declare: the host refuses it)
			if r.Form == nil {
				r.Form = map[string][]string{}
			}
			r.Form[k] = v
		}
	}
	return r, nil
}

// Param is an approved request's parameter, as text (a JSON string member without its quotes).
func Param(a protocol.Approved, k string) string {
	v := a.Params[k]
	if len(v) != 1 {
		return ""
	}
	if strings.HasPrefix(v[0], `"`) {
		var s string
		if json.Unmarshal([]byte(v[0]), &s) == nil {
			return s
		}
	}
	return v[0]
}

// JSONMembers makes the parameters of a JSON API call from Go values (each member in canonical
// form), for an approved request.
func JSONMembers(members map[string]any) (map[string][]string, error) {
	raw := map[string]jsontext.Value{}
	for k, v := range members {
		b, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return nil, fmt.Errorf("member %s: %w", k, err)
		}
		raw[k] = b
	}
	return protocol.JSONParams(raw)
}
