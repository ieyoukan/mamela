// Package fakegithub is an in-memory GitHub for mamelacd's tests: pull requests, their trees (as
// tarballs), the heads of branches, conversation comments and commit statuses.
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

// GitHub is the fake.
type GitHub struct {
	Repo  string
	Token string

	mu       sync.Mutex
	prs      []*PR
	trees    map[string][]byte // sha -> tarball
	branches map[string]string // name -> head
	comments []Comment
	statuses []Status
	next     int64
}

// New returns an empty fake of a repository that accepts the token.
func New(repo, token string) *GitHub {
	return &GitHub{Repo: repo, Token: token, trees: map[string][]byte{}, branches: map[string]string{}, next: 1000}
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
	g.prs = append(g.prs, &pr)
}

// Push moves a pull request's head.
func (g *GitHub) Push(number int, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prs {
		if p.Number == number {
			p.HeadSHA = sha
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
			g.branches[p.Base] = sha
		}
	}
}

// SetBranch moves the head of a branch (a push that is no pull request's merge).
func (g *GitHub) SetBranch(name, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[name] = sha
}

// Say comments on a pull request as someone.
func (g *GitHub) Say(pr int, author, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	g.comments = append(g.comments, Comment{ID: g.next, PR: pr, Author: author, Body: body})
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

func (g *GitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+g.Token {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	prefix := "/repos/" + g.Repo
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, prefix)
	g.mu.Lock()
	defer g.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.MarshalWrite(w, v)
	}
	switch {
	case r.Method == "GET" && p == "/pulls":
		var out []map[string]any
		for i := len(g.prs) - 1; i >= 0; i-- {
			pr := g.prs[i]
			state := "open"
			if pr.Merged {
				state = "closed"
			}
			if r.URL.Query().Get("state") != state {
				continue
			}
			head := g.Repo
			if pr.Fork {
				head = "someone/fork"
			}
			m := map[string]any{"number": pr.Number, "state": state, "title": "change", "user": map[string]any{"login": pr.Author, "type": "User"},
				"head": map[string]any{"ref": "topic", "sha": pr.HeadSHA, "repo": map[string]any{"full_name": head}},
				"base": map[string]any{"ref": pr.Base, "sha": strings.Repeat("0", 40), "repo": map[string]any{"full_name": g.Repo}}}
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
			g.next++
			g.comments = append(g.comments, Comment{ID: g.next, PR: n, Author: "mamela-bot", Bot: true, Body: body.Body})
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": g.next})
			return
		}
		var out []map[string]any
		for _, c := range g.comments {
			if c.PR == n {
				typ := "User"
				if c.Bot {
					typ = "Bot"
				}
				out = append(out, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": c.Author, "type": typ}})
			}
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
