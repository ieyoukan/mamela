// Package pin writes mamela.sum: it downloads what a declaration leaves to mamela.sum and records
// the sha256 of the bytes (`mamela pin`, docs/design/declaration-inputs.md, 2). It is the only part
// of Mamela that downloads for a declaration on a person's machine; evaluating never does.
//
// What it vouches for is the bytes as they were downloaded the first time (trust on first use),
// cross-checked, for a GitHub release's asset, with the digest GitHub lists for it. The defence is
// the person who reads the change of the version, the URL and the sha256 in review.
package pin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/download"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

// Limits of one download.
const (
	// MaxDownload is the most it downloads of one URL: what mamelet downloads at most of an
	// artifact (the linux extension's MaxArtifact).
	MaxDownload = 512 << 20
	// DownloadTimeout bounds one download, from the request to its last byte.
	DownloadTimeout = 10 * time.Minute
)

// Reason codes (docs/reason-codes.md, MAM-PIN).
const (
	CodeDownloadFailed    = "MAM-PIN-0101" // a download did not complete (network, HTTP status, time)
	CodeDownloadRefused   = "MAM-PIN-0102" // a download that is not https, redirects to http, or is too large
	CodeGitHubMismatch    = "MAM-PIN-0103" // a GitHub release's asset whose digest is another, or that the release does not list
	CodeGitHubUnavailable = "MAM-PIN-0104" // GitHub's API could not say what the release's asset is
	CodeSumChanged        = "MAM-PIN-0201" // mamela.sum changed while mamela pin ran
	CodeMissing           = "MAM-PIN-0301" // --check, --verify: mamela.sum lacks what the declaration needs
	CodeVerifyMismatch    = "MAM-PIN-0302" // --verify: a URL's bytes are no longer the ones pinned
	// CodeSumMalformed is evaluate.CodeSumMalformed: mamela pin reads the file as evaluating does.
	CodeSumMalformed = "MAM-EVAL-0602"
)

// Error is why mamela pin stopped, for a person: what, why, and what to do.
type Error struct {
	Code    string
	Message string
	Hint    string
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Message
	if e.Hint != "" {
		s += "\n  hint: " + e.Hint
	}
	return s
}

// Mode is what Run does.
type Mode int

const (
	// Update downloads each URL mamela.sum lacks and writes the file, without the lines no longer
	// needed.
	Update Mode = iota
	// Check reports what mamela.sum lacks and what it has that is not needed, without the network
	// and without writing.
	Check
	// Verify downloads every URL of mamela.sum again and compares, without writing.
	Verify
)

// Pinner pins with one HTTP client and its limits.
type Pinner struct {
	// Client makes the downloads and asks GitHub's API (nil: Go's default transport, the machine's
	// certificates). It is copied, and the copy follows redirects only to https.
	Client     *http.Client
	MaxBytes   int64         // 0: MaxDownload
	Timeout    time.Duration // of one download; 0: DownloadTimeout
	GitHubHost string        // the host of release downloads; "": github.com
	GitHubAPI  string        // "": https://api.github.com
}

// needed is one URL the declaration needs, and who needs it.
type needed struct {
	url  string
	pins []graph.Pin
}

func (n needed) key() bool {
	return slices.ContainsFunc(n.pins, func(p graph.Pin) bool { return p.Key })
}

func (n needed) who() string {
	var ids []string
	for _, p := range n.pins {
		if id := p.Resource + " " + p.Field; !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ", ")
}

// byURL gathers the pins by URL, in the order of the URLs.
func byURL(pins []graph.Pin) []needed {
	m := map[string]*needed{}
	var urls []string
	for _, p := range pins {
		n := m[p.URL]
		if n == nil {
			n = &needed{url: p.URL}
			m[p.URL] = n
			urls = append(urls, p.URL)
		}
		n.pins = append(n.pins, p)
	}
	slices.Sort(urls)
	out := make([]needed, len(urls))
	for i, u := range urls {
		out[i] = *m[u]
	}
	return out
}

// Run brings dir's mamela.sum to the pins (the sha256 the declaration in dir leaves to it:
// evaluate.Evaluator.Pins), as mode says, and tells out what it found and did.
func (p *Pinner) Run(ctx context.Context, dir string, pins []graph.Pin, mode Mode, out io.Writer) error {
	sums, before, err := sumfile.Read(dir)
	if err != nil {
		return malformed(dir, err)
	}
	need := byURL(pins)
	have := sums.Entries()
	var missing []needed
	for _, n := range need {
		if _, ok := sums.Lookup(n.url); !ok {
			missing = append(missing, n)
		}
	}
	var unused []sumfile.Entry
	for _, e := range have {
		if !slices.ContainsFunc(need, func(n needed) bool { return n.url == e.URL }) {
			unused = append(unused, e)
		}
	}
	switch mode {
	case Check:
		return p.check(dir, need, missing, unused, out)
	case Verify:
		return p.verify(ctx, dir, have, missing, unused, out)
	}
	return p.update(ctx, dir, before, have, missing, unused, out)
}

func malformed(dir string, err error) error {
	return &Error{Code: CodeSumMalformed, Message: err.Error() + "; nothing was downloaded or written",
		Hint: fmt.Sprintf("%s is written by `mamela pin`, not by hand: take it back from git (git checkout -- %s), "+
			"or remove it and run `mamela pin %s` again (which downloads everything again)", sumfile.Name, sumfile.Name, dir)}
}

func (p *Pinner) check(dir string, need, missing []needed, unused []sumfile.Entry, out io.Writer) error {
	for _, n := range missing {
		fmt.Fprintf(out, "missing  %s (%s)\n", n.url, n.who())
	}
	for _, e := range unused {
		fmt.Fprintf(out, "unused   %s sha256:%s\n", e.URL, e.SHA256)
	}
	if len(missing) > 0 {
		return missingError(dir, missing)
	}
	if len(unused) > 0 {
		fmt.Fprintf(out, "%s has every sha256 the declaration needs (%d), and %d line%s it no longer needs: `mamela pin %s` removes them\n",
			sumfile.Name, len(need), len(unused), plural(len(unused)), dir)
		return nil
	}
	fmt.Fprintf(out, "%s has every sha256 the declaration needs (%d)\n", sumfile.Name, len(need))
	return nil
}

func missingError(dir string, missing []needed) error {
	return &Error{Code: CodeMissing, Message: fmt.Sprintf("%s has no sha256 for %d URL%s the declaration downloads: evaluating it fails (MAM-EVAL-0601)",
		sumfile.Name, len(missing), plural(len(missing))),
		Hint: fmt.Sprintf("run `mamela pin %s`, check what it adds, and commit %s with the declaration", dir, sumfile.Name)}
}

func (p *Pinner) verify(ctx context.Context, dir string, have []sumfile.Entry, missing []needed, unused []sumfile.Entry, out io.Writer) error {
	var failed []error
	differ := 0
	for _, e := range have {
		got, note, err := p.sha256(ctx, e.URL)
		switch {
		case err != nil:
			fmt.Fprintf(out, "failed   %s: %v\n", e.URL, err)
			failed = append(failed, err)
		case got != e.SHA256:
			fmt.Fprintf(out, "DIFFERS  %s: mamela.sum has sha256:%s, the download now has sha256:%s\n", e.URL, e.SHA256, got)
			differ++
		default:
			fmt.Fprintf(out, "ok       %s%s\n", e.URL, note)
		}
	}
	for _, n := range missing {
		fmt.Fprintf(out, "missing  %s (%s)\n", n.url, n.who())
	}
	for _, e := range unused {
		fmt.Fprintf(out, "unused   %s\n", e.URL)
	}
	switch {
	case differ > 0:
		return &Error{Code: CodeVerifyMismatch, Message: fmt.Sprintf("the bytes at %d URL%s of %s are no longer the ones pinned (DIFFERS above); nothing was written",
			differ, plural(differ), sumfile.Name),
			Hint: "a published file should not change: find out why (a release published again, a mirror, a tampered download) before you trust it. " +
				"To take the new one, remove its line and run `mamela pin " + dir + "`, then review the change like a new version"}
	case len(failed) > 0:
		return failed[0]
	case len(missing) > 0:
		return missingError(dir, missing)
	}
	fmt.Fprintf(out, "%s: the bytes at its %d URL%s are the ones pinned\n", sumfile.Name, len(have), plural(len(have)))
	return nil
}

func (p *Pinner) update(ctx context.Context, dir string, before []byte, have []sumfile.Entry, missing []needed, unused []sumfile.Entry, out io.Writer) error {
	if len(missing) == 0 && len(unused) == 0 {
		fmt.Fprintf(out, "%s is up to date (%d sha256)\n", sumfile.Name, len(have))
		return nil
	}
	type added struct {
		needed
		sha256, note string
	}
	var adds []added
	for _, n := range missing {
		got, note, err := p.sha256(ctx, n.url)
		if err != nil {
			return wrapNothingWritten(err)
		}
		adds = append(adds, added{needed: n, sha256: got, note: note})
	}
	entries := slices.DeleteFunc(slices.Clone(have), func(e sumfile.Entry) bool {
		return slices.ContainsFunc(unused, func(u sumfile.Entry) bool { return u.URL == e.URL })
	})
	for _, a := range adds {
		entries = append(entries, sumfile.Entry{URL: a.url, SHA256: a.sha256})
	}
	data, err := sumfile.Format(entries)
	if err != nil {
		return err
	}
	if err := write(dir, before, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: %d added, %d removed\n", filepath.Join(dir, sumfile.Name), len(adds), len(unused))
	for _, a := range adds {
		fmt.Fprintf(out, "+ %s sha256:%s\n    for %s%s\n", a.url, a.sha256, a.who(), a.note)
		if a.key() {
			fmt.Fprintf(out, "    a signing key: mamela pin trusts the key it downloaded now (trust on first use). Before you commit, compare its\n"+
				"    fingerprint with the one its vendor publishes")
			if !strings.Contains(a.url, "'") { // shown as a command to copy: quoted for the shell
				fmt.Fprintf(out, ": curl -fsSL '%s' | gpg --show-keys", a.url)
			}
			fmt.Fprintln(out)
		}
	}
	for _, e := range unused {
		fmt.Fprintf(out, "- %s sha256:%s\n    no longer used by the declaration\n", e.URL, e.SHA256)
	}
	fmt.Fprintf(out, "review the change (git diff %s) and commit it with the declaration\n", sumfile.Name)
	return nil
}

// wrapNothingWritten adds to a download's error that the file was not written.
func wrapNothingWritten(err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		c := *pe
		c.Message += "; " + sumfile.Name + " was not written"
		return &c
	}
	return err
}

// write replaces dir's mamela.sum with data, when it is still what was read (before; nil: there was
// none): through a new file in the same directory, renamed over it.
func write(dir string, before, data []byte) error {
	_, now, err := sumfile.Read(dir)
	if err != nil {
		var se *sumfile.Error
		if !errors.As(err, &se) {
			return err
		}
		now = []byte("unreadable") // changed into something that is not a mamela.sum
	}
	if (before == nil) != (now == nil) || string(before) != string(now) {
		return &Error{Code: CodeSumChanged, Message: sumfile.Name + " changed while mamela pin ran; it was not written",
			Hint: "run mamela pin again (another mamela pin, an editor or a git checkout changed the file meanwhile)"}
	}
	tmp, err := os.CreateTemp(dir, "."+sumfile.Name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing once it is renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, sumfile.Name))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// client is the Pinner's client, following redirects only to https.
func (p *Pinner) client() *http.Client {
	c := &http.Client{}
	if p.Client != nil {
		*c = *p.Client
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = time.Minute
		c.Transport = t
	}
	download.HTTPSOnly(c, "mamela pin")
	return c
}

// sha256 downloads u and returns the sha256 of its bytes, cross-checked with GitHub for a release's
// asset (note says how).
func (p *Pinner) sha256(ctx context.Context, u string) (sum, note string, err error) {
	max := p.MaxBytes
	if max <= 0 {
		max = MaxDownload
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DownloadTimeout
	}
	c := p.client()
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sum, _, err = download.SHA256(dctx, c, u, max)
	if err != nil {
		var r *download.Refusal
		if errors.As(err, &r) {
			return "", "", &Error{Code: CodeDownloadRefused, Message: r.Reason,
				Hint: fmt.Sprintf("Mamela downloads over https only (a redirect too), at most %d MiB: declare a URL that is", max>>20)}
		}
		if dctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("download %s did not finish within %s", u, timeout)
		}
		return "", "", &Error{Code: CodeDownloadFailed, Message: err.Error(),
			Hint: "check the URL (and the version it is made of) and the network, then run mamela pin again"}
	}
	note, err = p.checkGitHub(ctx, c, u, sum)
	if err != nil {
		return "", "", err
	}
	return sum, note, nil
}
