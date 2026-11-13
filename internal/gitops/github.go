// Package gitops is mamelacd (ADR 0010, 0012): it follows a repository of declarations on GitHub.
// It plans each pull request of a trusted author and comments the plan, records an approval when
// an approver comments `/mamela approve <digest>`, and applies an approved plan once it is the
// declaration at the head of the branch. It holds a GitHub token and its own small database; it
// asks the Control Plane's local API (internal/api) to plan, approve and apply, and holds no
// credential of the infrastructure and none of Mamela's state. It reaches GitHub; nothing reaches it.
package gitops

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/tree"
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
	req.Header.Set("User-Agent", "mamelacd")
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

// Tarball fetches the tree of a commit: GitHub's gzipped tarball, at most tree.MaxTarball bytes.
// It is not extracted here: the Control Plane's API extracts it (internal/tree).
func (g *GitHub) Tarball(ctx context.Context, sha string) ([]byte, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("commit %q", sha)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(g.API, "/")+g.repoPath("/tarball/"+sha), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("User-Agent", "mamelacd")
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub tarball of %s: %s", sha, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tree.MaxTarball+1))
	if err != nil {
		return nil, err
	}
	if len(data) > tree.MaxTarball {
		return nil, fmt.Errorf("the tarball of %s is over %d bytes", sha, tree.MaxTarball)
	}
	return data, nil
}

// Branch returns the commit at the head of a branch.
func (g *GitHub) Branch(ctx context.Context, name string) (string, error) {
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/branches/"+url.PathEscape(name)), nil, nil, &out); err != nil {
		return "", err
	}
	if !validSHA(out.Commit.SHA) {
		return "", fmt.Errorf("the head of %s: commit %q", name, out.Commit.SHA)
	}
	return out.Commit.SHA, nil
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
