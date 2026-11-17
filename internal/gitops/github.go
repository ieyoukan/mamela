// Package gitops is mamelacd (ADR 0012; docs/gitops.md): it follows a repository of declarations
// on GitHub, and asks the Control Plane's local API (internal/api) to plan, approve and apply.
// It holds a GitHub token and its own small database, and no credential of the infrastructure
// and none of Mamela's state. It reaches GitHub; nothing reaches it.
package gitops

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	// TokenFile is where Token comes from (only to say so when GitHub refuses it).
	TokenFile string
	// Now is the clock (time.Now). It decides how long a rate limit still lasts.
	Now func() time.Time

	// limitedUntil: GitHub said the budget is used up until then; no request is sent before.
	limitedUntil time.Time
	limitErr     *RateLimitError
	// noCommitComments: GitHub refused to take a comment on a commit (the token lacks the
	// permission): it is not asked again until the token changes.
	noCommitComments bool
}

func (g *GitHub) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Reason codes of what GitHub does to mamelacd (docs/reason-codes.md, MAM-GIT).
const (
	CodeToken       = "MAM-GIT-0001" // the token is refused (401)
	CodePermission  = "MAM-GIT-0002" // the token is not allowed this (403 that is no rate limit)
	CodeRateLimit   = "MAM-GIT-0003" // GitHub's rate limit (403/429)
	CodeUnavailable = "MAM-GIT-0004" // GitHub answered 5xx
)

// APIError is GitHub's answer when it is not what was asked for, in one line a person can act on.
type APIError struct {
	Code    string
	Method  string
	Path    string
	Status  int
	Message string
	// Hint is what to do about it.
	Hint string
}

func (e *APIError) Error() string {
	var b strings.Builder
	if e.Code != "" {
		b.WriteString(e.Code + ": ")
	}
	fmt.Fprintf(&b, "GitHub %s %s answered %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		b.WriteString(" (" + e.Message + ")")
	}
	if e.Hint != "" {
		b.WriteString(": " + e.Hint)
	}
	return b.String()
}

// RateLimitError says GitHub's request budget is used up (primary) or that it asked for fewer
// requests at a time (secondary). Nothing is sent until Until.
type RateLimitError struct {
	Until     time.Time
	Wait      time.Duration // from when it was told
	Secondary bool
}

func (e *RateLimitError) Error() string {
	kind := "rate limit"
	if e.Secondary {
		kind = "secondary rate limit"
	}
	return fmt.Sprintf("%s: GitHub's %s is used up: reading again at %s (in %s); nothing is lost, the poll is only later",
		CodeRateLimit, kind, e.Until.UTC().Format("15:04:05Z"), e.Wait.Round(time.Second))
}

type PR struct {
	Number int  `json:"number"`
	User   User `json:"user"`
	Head   Ref  `json:"head"`
	Base   Ref  `json:"base"`
	// UpdatedAt moves when the pull request or a comment on it changes.
	UpdatedAt time.Time `json:"updated_at"`
	// MergedAt is set once it is merged; MergeCommitSHA is then the commit it was merged as.
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
}

type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type User struct {
	Login string `json:"login"`
	// ID is the account's numeric id: it stays when the login is changed, and a login that was left
	// can be taken by another account (B11).
	ID   int64  `json:"id"`
	Type string `json:"type"` // "User", "Bot", ...
}

type Comment struct {
	ID   int64  `json:"id"`
	User User   `json:"user"`
	Body string `json:"body"`
}

func (g *GitHub) do(ctx context.Context, method, p string, query url.Values, body any, out any) error {
	_, err := g.doResp(ctx, method, p, query, body, out)
	return err
}

// doResp is do, and returns the response's headers.
func (g *GitHub) doResp(ctx context.Context, method, p string, query url.Values, body any, out any) (http.Header, error) {
	if err := g.gate(); err != nil {
		return nil, err
	}
	u := strings.TrimSuffix(g.API, "/") + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	g.noteLimit(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, g.refusal(resp, method, p, data)
	}
	if out == nil {
		return resp.Header, nil
	}
	return resp.Header, json.Unmarshal(data, out)
}

// gate refuses to send a request while GitHub has said to wait: every request made while limited
// only makes the wait longer.
func (g *GitHub) gate() error {
	if now := g.now(); now.Before(g.limitedUntil) && g.limitErr != nil {
		e := *g.limitErr
		e.Wait = g.limitedUntil.Sub(now)
		return &e
	}
	return nil
}

// noteLimit remembers that the budget is used up when a response says so (the next request would
// be refused: it is not sent).
func (g *GitHub) noteLimit(resp *http.Response) {
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return
	}
	until := time.Unix(reset, 0)
	if until.After(g.now()) {
		g.limitedUntil = until
		g.limitErr = &RateLimitError{Until: until}
	}
}

// refusal turns a response that is no success into an error that says what to do. GitHub's own
// body is not shown as it is (it holds ids and links): its "message" is, cut short.
func (g *GitHub) refusal(resp *http.Response, method, p string, data []byte) error {
	var body struct {
		Message string `json:"message"`
	}
	json.Unmarshal(data, &body)
	msg := strings.Join(strings.Fields(body.Message), " ")
	if len([]rune(msg)) > 100 {
		msg = string([]rune(msg)[:99]) + "…"
	}
	if resp.StatusCode >= 500 {
		msg = "" // a gateway's page, not GitHub's message
	}
	e := &APIError{Method: method, Path: p, Status: resp.StatusCode, Message: msg}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden && g.rateLimited(resp, body.Message):
		return g.rateLimit(resp, body.Message)
	case resp.StatusCode == http.StatusUnauthorized:
		e.Code = CodeToken
		e.Message = ""
		e.Hint = "the token was revoked or has expired, or it has no access to " + g.Repo + ". Make a new token (Contents: read, Pull requests: read and write, Commit statuses: read and write)"
		if g.TokenFile != "" {
			e.Hint += ", write it to " + g.TokenFile + ", then send mamelacd a SIGHUP (systemctl kill -s HUP mamelacd) or restart it"
		}
	case resp.StatusCode == http.StatusForbidden:
		e.Code = CodePermission
		e.Hint = "the token is not allowed this: check its permissions (Contents: read, Pull requests: read and write, Commit statuses: read and write) and that it is for " + g.Repo
	case resp.StatusCode >= 500:
		e.Code = CodeUnavailable
		e.Hint = "GitHub's side; it is tried again at the next poll"
	}
	return e
}

// rateLimited says whether a 403 is GitHub's rate limit.
func (g *GitHub) rateLimited(resp *http.Response, message string) bool {
	return resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" ||
		strings.Contains(strings.ToLower(message), "rate limit")
}

// rateLimit is the error for a rate-limited answer, and holds the requests until it is over:
// Retry-After (seconds), else X-RateLimit-Reset, else a minute (a secondary limit asks for that).
func (g *GitHub) rateLimit(resp *http.Response, message string) error {
	now := g.now()
	var wait time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		wait = time.Duration(s) * time.Second
	} else if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		wait = time.Unix(reset, 0).Sub(now)
	}
	secondary := resp.Header.Get("X-RateLimit-Remaining") != "0" || strings.Contains(strings.ToLower(message), "secondary")
	if wait <= 0 {
		wait = time.Minute
	}
	e := &RateLimitError{Until: now.Add(wait), Wait: wait, Secondary: secondary}
	g.limitedUntil, g.limitErr = e.Until, e
	return e
}

// IsAuth says whether err is GitHub refusing the token (401).
func IsAuth(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == CodeToken
}

// IsRateLimit returns the rate limit err is, if it is one.
func IsRateLimit(err error) (*RateLimitError, bool) {
	var e *RateLimitError
	return e, errors.As(err, &e)
}

// fatal says whether err makes every later request of the poll fail too (the token is refused, or
// the budget is used up): the poll stops there instead of failing again for each pull request.
func fatal(err error) bool {
	_, limited := IsRateLimit(err)
	return limited || IsAuth(err)
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: time.Minute}
}

func (g *GitHub) repoPath(rest string) string { return "/repos/" + g.Repo + rest }

// PRs lists the pull requests in a state ("open", "closed"), the most recently updated first (the
// first page: n of them).
func (g *GitHub) PRs(ctx context.Context, state string, n int) ([]PR, error) {
	return g.PRsPage(ctx, state, n, 1)
}

// PRsPage is one page of PRs (pages count from 1; GitHub gives at most 100 to a page).
func (g *GitHub) PRsPage(ctx context.Context, state string, perPage, page int) ([]PR, error) {
	var out []PR
	q := url.Values{"state": {state}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {fmt.Sprint(perPage)}}
	if page > 1 {
		q.Set("page", fmt.Sprint(page))
	}
	return out, g.do(ctx, "GET", g.repoPath("/pulls"), q, nil, &out)
}

// maxOpenPages bounds the open pull requests read (100 to a page): more than that are not planned.
const maxOpenPages = 10

// OpenPRs lists all the open pull requests, the most recently updated first, a page at a time.
func (g *GitHub) OpenPRs(ctx context.Context) ([]PR, error) {
	var all []PR
	for page := 1; page <= maxOpenPages; page++ {
		prs, err := g.PRsPage(ctx, "open", 100, page)
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
		if len(prs) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("more than %d open pull requests: close the ones that are done", maxOpenPages*100)
}

// maxCommentPages bounds the comments read from one pull request (100 to a page).
const maxCommentPages = 50

// Comments lists all of a pull request's conversation comments, the oldest first, a page at a time.
// asOf is when GitHub answered the first page, by its own clock (zero when it gave none): what
// changed on the pull request after that shows in its updated_at.
func (g *GitHub) Comments(ctx context.Context, pr int) (cs []Comment, asOf time.Time, err error) {
	for page := 1; page <= maxCommentPages; page++ {
		var got []Comment
		q := url.Values{"per_page": {"100"}}
		if page > 1 {
			q.Set("page", fmt.Sprint(page))
		}
		h, err := g.doResp(ctx, "GET", g.repoPath(fmt.Sprintf("/issues/%d/comments", pr)), q, nil, &got)
		if err != nil {
			return nil, time.Time{}, err
		}
		if page == 1 {
			asOf, _ = http.ParseTime(h.Get("Date"))
		}
		cs = append(cs, got...)
		if len(got) < 100 {
			return cs, asOf, nil
		}
	}
	return nil, time.Time{}, fmt.Errorf("pull request #%d has more than %d comments", pr, maxCommentPages*100)
}

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
	if err := g.gate(); err != nil {
		return nil, err
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
	g.noteLimit(resp)
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, g.refusal(resp, "GET", g.repoPath("/tarball/"+sha), msg)
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

// CommitAuthor is the GitHub login (and numeric id) a commit is attributed to ("": none, or not a user).
func (g *GitHub) CommitAuthor(ctx context.Context, sha string) (login string, id int64, err error) {
	var out struct {
		Author *User `json:"author"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/commits/"+sha), nil, nil, &out); err != nil {
		return "", 0, err
	}
	if out.Author == nil || out.Author.Type != "User" {
		return "", 0, nil
	}
	return out.Author.Login, out.Author.ID, nil
}

// UserID is the numeric id of the account that has the login now. exists is false when none has.
func (g *GitHub) UserID(ctx context.Context, login string) (id int64, exists bool, err error) {
	var out User
	err = g.do(ctx, "GET", "/users/"+url.PathEscape(login), nil, nil, &out)
	var e *APIError
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return out.ID, out.ID != 0 && out.Login != "", nil
}

// CommentOnCommit comments on a commit (where there is no pull request to comment on). GitHub
// may need a permission the token is not given (Contents: write); then it says so once and the
// comment is left out: the reason is in the commit status and the log as well. ok is false then.
func (g *GitHub) CommentOnCommit(ctx context.Context, sha, body string) (ok bool, err error) {
	if g.noCommitComments {
		return false, nil
	}
	err = g.do(ctx, "POST", g.repoPath("/commits/"+sha+"/comments"), nil, map[string]string{"body": body}, nil)
	var e *APIError
	if errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Status == http.StatusNotFound) {
		g.noCommitComments = true
		return false, nil
	}
	return err == nil, err
}

// SetToken replaces the token (it was rotated).
func (g *GitHub) SetToken(token string) {
	g.Token = token
	g.noCommitComments = false
	g.limitedUntil, g.limitErr = time.Time{}, nil
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
