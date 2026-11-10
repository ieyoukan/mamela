package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Upstream sends a checked request to the API, with the credentials. The provider never sees
// either.
type Upstream interface {
	// Do sends the request. An error wrapping ErrNotSent guarantees it was not sent; any other
	// error means it may have taken effect.
	Do(ctx context.Context, r protocol.HostRequest) (status int, body []byte, err error)
}

// ErrNotSent marks an error that happened before the request was sent.
var ErrNotSent = errors.New("not sent")

// maxResponse bounds a response body.
const maxResponse = 8 << 20

// HTTP sends requests to an HTTPS API.
type HTTP struct {
	Base      string                // "https://pve.example:8006/api2/json"
	Client    *http.Client          // with timeouts
	Authorize func(r *http.Request) // adds the credentials
}

// Do implements Upstream.
func (h HTTP) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	if !strings.HasPrefix(h.Base, "https://") {
		return 0, nil, fmt.Errorf("%w: the API base must be https", ErrNotSent)
	}
	req, err := newRequest(ctx, h.Base, r)
	if err != nil {
		return 0, nil, err
	}
	if h.Authorize != nil {
		h.Authorize(req)
	}
	wrote := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true },
	}))
	// Never follow a redirect: the host checked this request, not the one a redirect asks for.
	client := *h.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if !wrote {
			return 0, nil, fmt.Errorf("%w: %v", ErrNotSent, err)
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading the response: %w", err)
	}
	if len(body) > maxResponse {
		return resp.StatusCode, nil, fmt.Errorf("a response larger than %d MiB", maxResponse>>20)
	}
	return resp.StatusCode, body, nil
}

func newRequest(ctx context.Context, base string, r protocol.HostRequest) (*http.Request, error) {
	u := base + r.Path
	if len(r.Query) > 0 {
		u += "?" + url.Values(r.Query).Encode()
	}
	var body io.Reader
	if len(r.Form) > 0 {
		body = strings.NewReader(url.Values(r.Form).Encode())
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req, nil
}

// Handler sends requests to an http.Handler in the same process: a fake API in tests.
type Handler struct {
	Handler http.Handler
}

// Do implements Upstream.
func (h Handler) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	req, err := newRequest(ctx, "http://api.test", r)
	if err != nil {
		return 0, nil, err
	}
	rec := httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, req)
	return rec.Code, bytes.Clone(rec.Body.Bytes()), nil
}
