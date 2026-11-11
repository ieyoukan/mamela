package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
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

// File is an artifact sent as the file parameter of a multipart request (ADR 0004).
type File struct {
	Field string   // the form parameter (the catalog's File)
	Order []string // the form fields in the order the call declares them; some APIs read them in order (pveproxy)
	Name  string   // the file name sent with it
	Size  int64
	Open  func() (io.ReadCloser, error)
}

// Uploader is an Upstream that can also send a file. An Upstream without it cannot upload.
type Uploader interface {
	Upload(ctx context.Context, r protocol.HostRequest, f File) (status int, body []byte, err error)
}

// reasonSink is a context key: where an Upstream may write the status line's text.
type reasonSink struct{}

// ErrNotSent marks an error that happened before the request was sent.
var ErrNotSent = errors.New("not sent")

// maxResponse bounds a response body.
const maxResponse = 8 << 20

// HTTP sends requests to an HTTPS API.
type HTTP struct {
	Base      string                      // "https://pve.example:8006/api2/json"
	Client    *http.Client                // with timeouts
	Authorize func(r *http.Request) error // adds the credentials (signs the request)
}

// Do implements Upstream.
func (h HTTP) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	return h.send(ctx, r, nil)
}

// Upload implements Uploader.
func (h HTTP) Upload(ctx context.Context, r protocol.HostRequest, f File) (int, []byte, error) {
	return h.send(ctx, r, &f)
}

func (h HTTP) send(ctx context.Context, r protocol.HostRequest, f *File) (int, []byte, error) {
	if err := CheckBase(h.Base); err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	req, err := newRequest(ctx, h.Base, r, f)
	if err != nil {
		return 0, nil, err
	}
	if h.Authorize != nil {
		if err := h.Authorize(req); err != nil {
			return 0, nil, fmt.Errorf("%w: authorize: %v", ErrNotSent, err)
		}
	}
	wrote := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true },
	}))
	// Never follow a redirect: the host checked this request, not the one a redirect asks for.
	client := *h.Client
	if f != nil {
		// An upload of a large file takes as long as it takes; the provider call's deadline (the
		// request's context) still bounds it.
		client.Timeout = 0
	}
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
	// Some APIs (Proxmox) say why they refused in the status line, not in the body.
	if r, ok := ctx.Value(reasonSink{}).(*string); ok && resp.StatusCode >= 400 {
		*r = strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	}
	return resp.StatusCode, body, nil
}

// CheckBase accepts an https API base, or http on the loopback interface only (a local API, as
// in development): credentials never cross a network in clear text.
func CheckBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("API base %q is not a plain URL", base)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost"):
		return nil
	}
	return fmt.Errorf("API base %q must be https (http only on 127.0.0.1, ::1 or localhost)", base)
}

func newRequest(ctx context.Context, base string, r protocol.HostRequest, f *File) (*http.Request, error) {
	u := base + r.Path
	if len(r.Query) > 0 {
		u += "?" + url.Values(r.Query).Encode()
	}
	if f != nil {
		return multipartRequest(ctx, u, r, *f)
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

// multipartRequest streams the form fields, then the file, with its exact length (pveproxy and
// other APIs reject chunked uploads).
func multipartRequest(ctx context.Context, u string, r protocol.HostRequest, f File) (*http.Request, error) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	// The fields go first, in the declared order (then any other, sorted), and the file last:
	// pveproxy reads content, checksum-algorithm and checksum in that order before the file.
	keys := slices.DeleteFunc(slices.Clone(f.Order), func(k string) bool { _, ok := r.Form[k]; return !ok })
	for _, k := range slices.Sorted(maps.Keys(r.Form)) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		for _, v := range r.Form[k] {
			if err := mw.WriteField(k, v); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
			}
		}
	}
	if _, err := mw.CreateFormFile(f.Field, f.Name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	content, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: open artifact %s: %v", ErrNotSent, f.Name, err)
	}
	body := io.MultiReader(bytes.NewReader(head.Bytes()), content, strings.NewReader(tail))
	req, err := http.NewRequestWithContext(ctx, r.Method, u, struct {
		io.Reader
		io.Closer
	}{body, content})
	if err != nil {
		content.Close()
		return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	req.ContentLength = int64(head.Len()) + f.Size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req, nil
}

// Handler sends requests to an http.Handler in the same process: a fake API in tests.
type Handler struct {
	Handler http.Handler
}

// Do implements Upstream.
func (h Handler) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	return h.send(ctx, r, nil)
}

// Upload implements Uploader.
func (h Handler) Upload(ctx context.Context, r protocol.HostRequest, f File) (int, []byte, error) {
	return h.send(ctx, r, &f)
}

func (h Handler) send(ctx context.Context, r protocol.HostRequest, f *File) (int, []byte, error) {
	req, err := newRequest(ctx, "http://api.test", r, f)
	if err != nil {
		return 0, nil, err
	}
	rec := httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, req)
	return rec.Code, bytes.Clone(rec.Body.Bytes()), nil
}
