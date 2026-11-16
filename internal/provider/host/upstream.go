package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

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

// ErrCertificatePin is what the check of a pinned TLS certificate (tls_fingerprint) returns when the
// server's certificate is not the pinned one.
var ErrCertificatePin = errors.New("the server certificate does not match the pinned fingerprint")

// maxResponse bounds a response body.
const maxResponse = 8 << 20

// HTTP sends requests to an HTTPS API.
type HTTP struct {
	Base      string                      // "https://pve.example:8006/api2/json"
	Client    *http.Client                // with timeouts
	Authorize func(r *http.Request) error // adds the credentials (signs the request)
	// Detail receives the failure of a request as Go reports it, with the address in it, for the
	// operator's log. The error the host returns says what kind of failure it was and never where:
	// a plan's errors reach a pull request (B10 of docs/reviews/2026-11-17-iac-audit.md).
	Detail func(err error)
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
		h.detail(err)
		return 0, nil, notAURL(err)
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
		h.detail(err)
		if !wrote {
			return 0, nil, fmt.Errorf("%w: %s", ErrNotSent, sayConnectionFailure(err))
		}
		return 0, nil, errors.New(sayConnectionFailure(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		h.detail(err)
		return resp.StatusCode, nil, errors.New(sayConnectionFailure(err) + " while reading the response")
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

func (h HTTP) detail(err error) {
	if h.Detail != nil {
		h.Detail(err)
	}
}

// CheckBase accepts an https API base, or http on the loopback interface only (a local API, as
// in development): credentials never cross a network in clear text. Its errors do not repeat the
// base: they reach plans and pull requests, and the section of mamela.providers.toml that has the
// base is named by whoever wraps them.
func CheckBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("api_url is not a plain URL (no query, fragment or user name)")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost"):
		return nil
	}
	return errors.New("api_url must be https (http only on 127.0.0.1, ::1 or localhost)")
}

// connectionCode is the reason code of a failed request to the API.
const connectionCode = "MAM-PRV-0012"

// sayConnectionFailure tells what kind of failure a request to the API met, for a person and for a
// pull request: never the address, which Go's errors carry (the URL, the host, the port, the
// resolver). The detail goes to the operator's log (HTTP.Detail).
func sayConnectionFailure(err error) string {
	var (
		dns     *net.DNSError
		certErr *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		name    x509.HostnameError
		invalid x509.CertificateInvalidError
		timeout interface{ Timeout() bool }
		why     string
		fix     = "check that the API is up and that api_url in mamela.providers.toml is right"
	)
	switch {
	case errors.Is(err, ErrCertificatePin):
		why, fix = "the server certificate does not match the pinned fingerprint", "check tls_fingerprint in mamela.providers.toml, and whether the API's certificate was renewed"
	case errors.As(err, &certErr), errors.As(err, &unknown), errors.As(err, &name), errors.As(err, &invalid):
		why, fix = "the server's TLS certificate was not accepted", "give tls_fingerprint in mamela.providers.toml for a self-signed certificate, or use a certificate a CA signed"
	case errors.As(err, &dns):
		why, fix = "the name of the API's host did not resolve", "check api_url in mamela.providers.toml and the Control Plane's DNS"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		why = "the API did not answer in time"
	case errors.Is(err, context.Canceled):
		why, fix = "the request was canceled", "run it again"
	case errors.Is(err, syscall.ECONNREFUSED):
		why = "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET):
		why = "the connection was reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		why, fix = "there is no route to the API", "check the Control Plane's network and api_url in mamela.providers.toml"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		why = "the API closed the connection without an answer"
	default:
		why = "the connection failed"
	}
	return connectionCode + ": " + why + "; " + fix + " (the API's address is in the Control Plane's log, not here)"
}

// notAURL is the error of a request that could not be made: the base or the path is not a URL. Go's
// error repeats the URL.
func notAURL(err error) error {
	if errors.Is(err, ErrNotSent) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%w: %s: the request is not a valid URL (%v)", ErrNotSent, connectionCode, err)
}

// addresses are what Redact removes: URLs, and the address of a connection (IPv4 with a port, IPv6
// in brackets). An address with no port is left: it may be a value of the declaration that a person
// has to compare (a refused request names the parameter and its value).
var addresses = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>)\]}]*[^\s"'<>)\]}.,;:]|\[[0-9a-f:.]*:[0-9a-f:.]*\](?::\d+)?|\b(?:\d{1,3}\.){3}\d{1,3}:\d+\b`)

// Redact removes what looks like where an API is from text that may reach a pull request or anyone
// who does not run the Control Plane: a URL, the address and port of a connection. It is the last
// line of defence; the errors of this package do not carry addresses to begin with.
func Redact(s string) string { return addresses.ReplaceAllString(s, "<address removed>") }

func newRequest(ctx context.Context, base string, r protocol.HostRequest, f *File) (*http.Request, error) {
	u := base + r.Path
	if len(r.Query) > 0 {
		u += "?" + url.Values(r.Query).Encode()
	}
	if f != nil {
		return multipartRequest(ctx, u, r, *f)
	}
	var body io.Reader
	contentType := ""
	switch {
	case r.JSON != nil:
		// An AWS JSON API: the members, each in canonical form (what was checked), as one object.
		b, err := jsonBody(r.JSON)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
		}
		body, contentType = bytes.NewReader(b), "application/x-amz-json-1.1"
	case len(r.Form) > 0:
		body, contentType = strings.NewReader(url.Values(r.Form).Encode()), "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, k := range slices.Sorted(maps.Keys(r.Header)) {
		for _, v := range r.Header[k] {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// jsonBody is a JSON object of the members, each in canonical form, in sorted order.
func jsonBody(members map[string]jsontext.Value) ([]byte, error) {
	params, err := protocol.JSONParams(members)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(params)) {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		b.WriteString(params[k][0])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
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
