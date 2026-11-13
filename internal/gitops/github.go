// Package gitops lets the Control Plane follow a repository of declarations on GitHub (ADR 0010):
// it plans each pull request of a trusted author and comments the plan, approves a plan when an
// approver comments `/mamela approve <digest>`, and applies it once the pull request is merged and
// the merged declaration is the one approved. It reaches GitHub; nothing reaches it.
package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// GitHub is the part of GitHub's REST API gitops uses.
type GitHub struct {
	API   string // https://api.github.com
	Repo  string // owner/name
	Token string
	HTTP  *http.Client
}

// PR is a pull request.
type PR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	User   User   `json:"user"`
	Head   Ref    `json:"head"`
	Base   Ref    `json:"base"`
	// MergedAt is set once it is merged; MergeCommitSHA is then the commit it was merged as.
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
}

// Ref is a pull request's head or base.
type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "User", "Bot", ...
}

// Comment is a comment on a pull request's conversation.
type Comment struct {
	ID   int64  `json:"id"`
	User User   `json:"user"`
	Body string `json:"body"`
}

func (g *GitHub) do(ctx context.Context, method, p string, query url.Values, body any, out any) error {
	u := strings.TrimSuffix(g.API, "/") + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "mamela-gitops")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub %s %s: %s: %.300s", method, p, resp.Status, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: time.Minute}
}

func (g *GitHub) repoPath(rest string) string { return "/repos/" + g.Repo + rest }

// PRs lists the pull requests in a state ("open", "closed"), the most recently updated first.
func (g *GitHub) PRs(ctx context.Context, state string, n int) ([]PR, error) {
	var out []PR
	q := url.Values{"state": {state}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {fmt.Sprint(n)}}
	return out, g.do(ctx, "GET", g.repoPath("/pulls"), q, nil, &out)
}

// Comments lists a pull request's conversation comments (up to 100, the oldest first).
func (g *GitHub) Comments(ctx context.Context, pr int) ([]Comment, error) {
	var out []Comment
	return out, g.do(ctx, "GET", g.repoPath(fmt.Sprintf("/issues/%d/comments", pr)), url.Values{"per_page": {"100"}}, nil, &out)
}

// Comment comments on a pull request.
func (g *GitHub) Comment(ctx context.Context, pr int, body string) error {
	return g.do(ctx, "POST", g.repoPath(fmt.Sprintf("/issues/%d/comments", pr)), nil, map[string]string{"body": body}, nil)
}

// Status sets a commit status ("pending", "success", "failure", "error").
func (g *GitHub) Status(ctx context.Context, sha, state, context, description string) error {
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	return g.do(ctx, "POST", g.repoPath("/statuses/"+sha), nil,
		map[string]string{"state": state, "context": context, "description": description}, nil)
}

// Limits on a fetched tree.
const (
	maxTarball = 64 << 20  // compressed
	maxTree    = 256 << 20 // uncompressed
	maxFiles   = 20000
)

// Checkout fetches the tree of a commit (its tarball) into dir, which must not exist. Only
// regular files and directories are written, under dir: a path that leaves it, a link or any
// other entry refuses the whole tree.
func (g *GitHub) Checkout(ctx context.Context, sha, dir string) error {
	if !validSHA(sha) {
		return fmt.Errorf("commit %q", sha)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(g.API, "/")+g.repoPath("/tarball/"+sha), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("User-Agent", "mamela-gitops")
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub tarball of %s: %s", sha, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTarball+1))
	if err != nil {
		return err
	}
	if len(data) > maxTarball {
		return fmt.Errorf("the tarball of %s is over %d bytes", sha, maxTarball)
	}
	return Extract(bytes.NewReader(data), dir)
}

// Extract writes a GitHub tarball (one top directory) into dir, which must not exist.
func Extract(r io.Reader, dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s exists", dir)
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	tmp := dir + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp) // a refused or broken tree is not kept
		}
	}()
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // GitHub's comment with the commit
		}
		// "<owner>-<repo>-<sha>/path": the top directory is dropped. No segment may climb.
		if slices.Contains(strings.Split(h.Name, "/"), "..") || path.IsAbs(h.Name) {
			return fmt.Errorf("tarball entry %q leaves the tree", h.Name)
		}
		name := path.Clean(h.Name)
		_, rest, ok := strings.Cut(name, "/")
		if !ok || rest == "" {
			continue
		}
		if path.IsAbs(rest) || rest == ".." || strings.HasPrefix(rest, "../") || strings.Contains(rest, "\\") {
			return fmt.Errorf("tarball entry %q leaves the tree", h.Name)
		}
		target := filepath.Join(tmp, filepath.FromSlash(rest))
		if files++; files > maxFiles {
			return fmt.Errorf("the tree has over %d files", maxFiles)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > maxTree {
				return fmt.Errorf("the tree is over %d bytes", maxTree)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("tarball entry %q is a link or a special file: the tree is refused", h.Name)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	done = true
	return nil
}

func validSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
