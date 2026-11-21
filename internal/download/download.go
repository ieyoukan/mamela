// Package download has what the downloads pinned by sha256 share, wherever they are made: by
// mamelet on a node (the linux extension's NodeArtifact and NodeAptRepo, internal/ext/linux) and by
// `mamela pin` on a person's machine. A download goes over https only (a redirect too) and is
// bounded in size and time; it runs no program.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Refusal is an error that says what was given is refused (a download that is not the approved
// one, a redirect to http, a download too large), not that something failed on the way and could
// work another time.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Refuse is a Refusal of the formatted reason.
func Refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// MaxRedirects bounds the redirects one download follows.
const MaxRedirects = 10

// HTTPSOnly makes c follow a redirect only to https, MaxRedirects at most: anything else is a
// Refusal, whose reason names who downloads ("mamelet").
func HTTPSOnly(c *http.Client, who string) {
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		switch {
		case req.URL.Scheme != "https":
			return Refuse("%s redirects to %s, which is not https: %s downloads over https only", via[0].URL.Redacted(), req.URL.Redacted(), who)
		case len(via) >= MaxRedirects:
			return Refuse("%s redirects more than %d times", via[0].URL.Redacted(), MaxRedirects)
		}
		return nil
	}
}

// SHA256 downloads rawURL (https) with client, which follows redirects only to https (HTTPSOnly),
// and returns the sha256 (hex) of its bytes and their number. The body is asked for as it is (no
// transparent decompression), so that its sha256 is the one a release's checksum file lists. A
// download of more than max bytes, or that does not come over https, is a Refusal; nothing of it is
// kept.
func SHA256(ctx context.Context, client *http.Client, rawURL string, max int64) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	if req.URL.Scheme != "https" {
		return "", 0, Refuse("%s is not https: downloads go over https only", req.URL.Redacted())
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return "", 0, r
		}
		return "", 0, fmt.Errorf("download %s: %w", req.URL.Redacted(), err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode != http.StatusOK:
		return "", 0, fmt.Errorf("download %s: the server answered %s", req.URL.Redacted(), resp.Status)
	case resp.Request != nil && resp.Request.URL.Scheme != "https":
		return "", 0, Refuse("download %s: it came over %s, not https", req.URL.Redacted(), resp.Request.URL.Scheme)
	case resp.ContentLength > max:
		return "", 0, Refuse("download %s: it is %d bytes, more than the %d MiB downloaded at most", req.URL.Redacted(), resp.ContentLength, max>>20)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, max+1))
	switch {
	case err != nil:
		return "", n, fmt.Errorf("download %s: %w (after %d bytes)", req.URL.Redacted(), err, n)
	case n > max:
		return "", n, Refuse("download %s: it is larger than the %d MiB downloaded at most", req.URL.Redacted(), max>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
