// Package fakegithub is an in-memory GitHub for mamelacd's tests: pull requests, their trees (as
// tarballs), the heads of branches, conversation comments, commit comments and commit statuses.
//
// It imitates what GitHub does that a client can get wrong, because a fake that is kinder than
// GitHub lets defects through (docs/reviews/2026-11-17-iac-audit.md, A7):
//
//   - lists are paged (per_page: 30 by default, 100 at most; a Link header names the next page);
//   - every account has a numeric id that outlives its login: Rename frees a login, which somebody
//     else can then take (Register);
//   - pull requests have an updated_at that a comment moves, and the Date header is the fake's
//     clock (which ticks one second per request and per event);
//   - requests can be counted (Requests), failed on purpose (Fail: a 502, a 401, a secondary rate
//     limit with Retry-After) and limited (SetRateLimit: 403 and X-RateLimit-* once it is used up).
package fakegithub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PR is a pull request of the fake.
type PR struct {
	Number         int
	Author         string
	Fork           bool
	Base           string
	HeadSHA        string
	Merged         bool
	MergeCommitSHA string

	updated time.Time // set by the fake; a comment moves it
	order   int
}

// Comment is a comment of the fake.
type Comment struct {
	ID     int64
	PR     int
	Author string
	Bot    bool
	Body   string
}

// Status is a commit status the fake received.
type Status struct {
	SHA, State, Context, Description string
}

// CommitComment is a commit comment the fake received.
type CommitComment struct {
	SHA, Body string
}

// failure is a response to give instead of the real one.
type failure struct {
	method, prefix string
	status         int
	message        string
	header         map[string]string
	times          int
}

// GitHub is the fake.
type GitHub struct {
	Repo  string
	token string

	mu            sync.Mutex
	prs           []*PR
	trees         map[string][]byte // sha -> tarball
	branches      map[string]string // name -> head
	authors       map[string]string // sha -> the login a commit is attributed to
	comments      []Comment
	updatedAt     map[int]time.Time // pull request number -> the last comment on it
	statuses      []Status
	commitNotes   []CommitComment
	denyCommitCom bool
	next          int64
	now           time.Time
	users         map[string]int64 // login -> numeric id
	gone          map[string]bool  // deleted accounts: 404
	nextUser      int64
	requests      []string
	failures      []*failure
	limit         int // 0: unlimited
	used          int
	resetAt       time.Time
	events        int
}

// New returns an empty fake of a repository that accepts the token.
func New(repo, token string) *GitHub {
	return &GitHub{Repo: repo, token: token, trees: map[string][]byte{}, branches: map[string]string{}, authors: map[string]string{},
		updatedAt: map[int]time.Time{}, users: map[string]int64{}, gone: map[string]bool{}, next: 1000, nextUser: 5000,
		now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
}

// SetToken changes the token the fake accepts (a token revoked and replaced).
func (g *GitHub) SetToken(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.token = token
}

// tick moves the fake's clock one second on; the caller holds mu.
func (g *GitHub) tick() time.Time {
	g.now = g.now.Add(time.Second)
	return g.now
}

// Now is the fake's clock.
func (g *GitHub) Now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now
}

// Advance moves the fake's clock on (the time between polls).
func (g *GitHub) Advance(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = g.now.Add(d)
}

// Register creates an account with the login (a login that is free) and returns its numeric id.
// Every login that is mentioned without being registered is registered when it is first seen.
func (g *GitHub) Register(login string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.gone, login)
	return g.register(login)
}

func (g *GitHub) register(login string) int64 {
	if id, ok := g.users[login]; ok {
		return id
	}
	g.nextUser++
	g.users[login] = g.nextUser
	return g.nextUser
}

// UserID is the numeric id of the account that has the login now (0: none).
func (g *GitHub) UserID(login string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.users[login]
}

// Rename gives an account another login; the old login is free (Register takes it for a new
// account, with another id).
func (g *GitHub) Rename(from, to string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.register(from)
	delete(g.users, from)
	g.users[to] = id
	// What the account did shows under the new login.
	for _, p := range g.prs {
		if p.Author == from {
			p.Author = to
		}
	}
	for i := range g.comments {
		if g.comments[i].Author == from {
			g.comments[i].Author = to
		}
	}
	for sha, login := range g.authors {
		if login == from {
			g.authors[sha] = to
		}
	}
}

// Delete removes the account with the login: it does not exist any more (404).
func (g *GitHub) Delete(login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.users, login)
	g.gone[login] = true
}

// AddTree makes a commit whose tree is the directory dir (files only), and returns its SHA.
func (g *GitHub) AddTree(dir string) (string, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	g.mu.Lock()
	g.next++
	sha := fmt.Sprintf("%040x", g.next)
	g.mu.Unlock()
	top := "owner-repo-" + sha[:7]
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return "", err
	}
	tw.Close()
	zw.Close()
	g.mu.Lock()
	g.trees[sha] = buf.Bytes()
	g.mu.Unlock()
	return sha, nil
}

// AddTarball makes a commit with a given tarball (for refused trees).
func (g *GitHub) AddTarball(data []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	sha := fmt.Sprintf("%040x", g.next)
	g.trees[sha] = data
	return sha
}

// Open opens a pull request.
func (g *GitHub) Open(pr PR) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events++
	pr.order = g.events
	pr.updated = g.tick()
	g.register(pr.Author)
	g.prs = append(g.prs, &pr)
}

// Push moves a pull request's head.
func (g *GitHub) Push(number int, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Number == number {
			p.HeadSHA = sha
			p.updated = g.tick()
		}
	}
}

// Merge merges a pull request as the commit sha, which becomes the head of its base.
func (g *GitHub) Merge(number int, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Number == number {
			p.Merged, p.MergeCommitSHA = true, sha
			p.updated = g.tick()
			g.branches[p.Base] = sha
		}
	}
}

// Touch moves a pull request's updated_at (somebody commented, labelled or edited it).
func (g *GitHub) Touch(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Number == number {
			p.updated = g.tick()
		}
	}
}

// SetBranch moves the head of a branch (a push that is no pull request's merge).
func (g *GitHub) SetBranch(name, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[name] = sha
}

// SetAuthor says whose commit sha is (a GitHub user; without it, a commit is attributed to nobody).
func (g *GitHub) SetAuthor(sha, login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authors[sha] = login
	g.register(login)
}

// Say comments on a pull request as someone.
func (g *GitHub) Say(pr int, author, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.register(author)
	g.addComment(Comment{PR: pr, Author: author, Body: body})
}

func (g *GitHub) addComment(c Comment) int64 {
	g.next++
	c.ID = g.next
	g.comments = append(g.comments, c)
	t := g.tick()
	for _, p := range g.prs {
		if p.Number == c.PR {
			p.updated = t
		}
	}
	return c.ID
}

// Comments returns a pull request's comments.
func (g *GitHub) Comments(pr int) []Comment {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Comment
	for _, c := range g.comments {
		if c.PR == pr {
			out = append(out, c)
		}
	}
	return out
}

// Statuses returns the statuses received for a commit.
func (g *GitHub) Statuses(sha string) []Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Status
	for _, s := range g.statuses {
		if s.SHA == sha {
			out = append(out, s)
		}
	}
	return out
}

// CommitComments returns the commit comments received for a commit.
func (g *GitHub) CommitComments(sha string) []CommitComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []CommitComment
	for _, c := range g.commitNotes {
		if c.SHA == sha {
			out = append(out, c)
		}
	}
	return out
}

// DenyCommitComments makes commit comments answer 403 (a token without the permission).
func (g *GitHub) DenyCommitComments() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.denyCommitCom = true
}

// Requests returns the requests received so far ("GET /repos/o/r/pulls?page=1&state=open"), oldest first.
func (g *GitHub) Requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// CountRequests counts the requests whose "METHOD path" starts with prefix ("" counts them all).
func (g *GitHub) CountRequests(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// ResetRequests forgets the requests counted so far.
func (g *GitHub) ResetRequests() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = nil
}

// Fail makes the next `times` requests of the method (path starting with prefix, relative to the
// repository: "/issues/", "" for any) answer with the status and message. header is added to the
// response (Retry-After, X-RateLimit-Reset...). Use it for a 502, a 401, a secondary rate limit.
func (g *GitHub) Fail(method, prefix string, status, times int, message string, header map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = append(g.failures, &failure{method: method, prefix: prefix, status: status, times: times, message: message, header: header})
}

// SetRateLimit allows `limit` requests until resetAt (the fake's clock): then it answers 403 with
// X-RateLimit-Remaining: 0 and X-RateLimit-Reset, as GitHub does when the hour's budget is used up.
// The budget is full again once the fake's clock is past resetAt. Every response carries the
// X-RateLimit-* headers while a limit is set.
func (g *GitHub) SetRateLimit(limit int, resetAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit, g.used, g.resetAt = limit, 0, resetAt
}

// userJSON is how an account shows in a response: the login now, and the id that stays.
func (g *GitHub) userJSON(login string) map[string]any {
	return map[string]any{"login": login, "id": g.register(login), "type": "User"}
}

func (g *GitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.tick()
	g.requests = append(g.requests, r.Method+" "+r.URL.RequestURI())
	w.Header().Set("Date", now.UTC().Format(http.TimeFormat))
	fail := func(status int, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.MarshalWrite(w, map[string]string{"message": message})
	}
	if g.limit > 0 {
		if !now.Before(g.resetAt) {
			g.used = 0 // a new hour
			g.resetAt = now.Add(time.Hour)
		}
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(g.limit))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(g.resetAt.Unix(), 10))
		if g.used >= g.limit {
			w.Header().Set("X-RateLimit-Remaining", "0")
			fail(http.StatusForbidden, "API rate limit exceeded for user ID 12345.")
			return
		}
		g.used++
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(g.limit-g.used))
	}
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		fail(http.StatusUnauthorized, "Bad credentials")
		return
	}
	prefix := "/repos/" + g.Repo
	rel := strings.TrimPrefix(r.URL.Path, prefix)
	for _, f := range g.failures {
		if f.times > 0 && f.method == r.Method && strings.HasPrefix(rel, f.prefix) {
			f.times--
			for k, v := range f.header {
				w.Header().Set(k, v)
			}
			fail(f.status, f.message)
			return
		}
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.MarshalWrite(w, v)
	}
	if login, ok := strings.CutPrefix(r.URL.Path, "/users/"); ok && r.Method == "GET" {
		id, exists := g.users[login]
		if !exists && !g.gone[login] {
			id, exists = g.register(login), true // a login that was never mentioned: registered at first sight
		}
		if !exists {
			fail(http.StatusNotFound, "Not Found")
			return
		}
		reply(map[string]any{"login": login, "id": id, "type": "User"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	p := rel
	// page returns the slice of the page asked for (per_page: 30 by default, 100 at most) and sets
	// the Link header to the next page.
	page := func(n int) (from, to int) {
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if perPage <= 0 {
			perPage = 30
		}
		perPage = min(perPage, 100)
		pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if pg <= 0 {
			pg = 1
		}
		from = min((pg-1)*perPage, n)
		to = min(from+perPage, n)
		if to < n {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(pg+1))
			q.Set("per_page", strconv.Itoa(perPage))
			w.Header().Set("Link", fmt.Sprintf("<http://%s%s?%s>; rel=\"next\"", r.Host, r.URL.Path, q.Encode()))
		}
		return from, to
	}
	switch {
	case r.Method == "GET" && p == "/pulls":
		want := r.URL.Query().Get("state")
		var match []*PR
		for _, pr := range g.prs {
			state := "open"
			if pr.Merged {
				state = "closed"
			}
			if want == state {
				match = append(match, pr)
			}
		}
		// The most recently updated first (sort=updated&direction=desc), as GitHub lists them.
		slices.SortStableFunc(match, func(a, b *PR) int {
			if c := b.updated.Compare(a.updated); c != 0 {
				return c
			}
			return b.order - a.order
		})
		from, to := page(len(match))
		out := []map[string]any{}
		for _, pr := range match[from:to] {
			head := g.Repo
			if pr.Fork {
				head = "someone/fork"
			}
			state := "open"
			if pr.Merged {
				state = "closed"
			}
			m := map[string]any{"number": pr.Number, "state": state, "title": "change", "user": g.userJSON(pr.Author),
				"updated_at": pr.updated.UTC().Format(time.RFC3339),
				"head":       map[string]any{"ref": "topic", "sha": pr.HeadSHA, "repo": map[string]any{"full_name": head}},
				"base":       map[string]any{"ref": pr.Base, "sha": strings.Repeat("0", 40), "repo": map[string]any{"full_name": g.Repo}}}
			if pr.Merged {
				m["merged_at"] = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
				m["merge_commit_sha"] = pr.MergeCommitSHA
			}
			out = append(out, m)
		}
		reply(out)
	case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
		sha, ok := g.branches[strings.TrimPrefix(p, "/branches/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reply(map[string]any{"name": strings.TrimPrefix(p, "/branches/"), "commit": map[string]any{"sha": sha}})
	case strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/comments") && r.Method == "POST":
		if g.denyCommitCom {
			fail(http.StatusForbidden, "Resource not accessible by personal access token")
			return
		}
		sha := strings.TrimSuffix(strings.TrimPrefix(p, "/commits/"), "/comments")
		var body struct {
			Body string `json:"body"`
		}
		json.UnmarshalRead(r.Body, &body)
		g.commitNotes = append(g.commitNotes, CommitComment{SHA: sha, Body: body.Body})
		w.WriteHeader(http.StatusCreated)
		reply(map[string]any{"id": len(g.commitNotes)})
	case r.Method == "GET" && strings.HasPrefix(p, "/commits/"):
		sha := strings.TrimPrefix(p, "/commits/")
		if login := g.authors[sha]; login != "" {
			reply(map[string]any{"sha": sha, "author": g.userJSON(login)})
		} else {
			reply(map[string]any{"sha": sha, "author": nil})
		}
	case r.Method == "GET" && strings.HasPrefix(p, "/tarball/"):
		data, ok := g.trees[strings.TrimPrefix(p, "/tarball/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	case strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == "POST" {
			var body struct {
				Body string `json:"body"`
			}
			json.UnmarshalRead(r.Body, &body)
			id := g.addComment(Comment{PR: n, Author: "mamela-bot", Bot: true, Body: body.Body})
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": id})
			return
		}
		var mine []Comment
		for _, c := range g.comments {
			if c.PR == n {
				mine = append(mine, c)
			}
		}
		from, to := page(len(mine))
		out := []map[string]any{}
		for _, c := range mine[from:to] {
			u := g.userJSON(c.Author)
			if c.Bot {
				u["type"] = "Bot"
			}
			out = append(out, map[string]any{"id": c.ID, "body": c.Body, "user": u})
		}
		reply(out)
	case r.Method == "POST" && strings.HasPrefix(p, "/statuses/"):
		var body struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"`
		}
		json.UnmarshalRead(r.Body, &body)
		g.statuses = append(g.statuses, Status{SHA: strings.TrimPrefix(p, "/statuses/"), State: body.State, Context: body.Context, Description: body.Description})
		w.WriteHeader(http.StatusCreated)
		reply(map[string]any{})
	default:
		http.NotFound(w, r)
	}
}
