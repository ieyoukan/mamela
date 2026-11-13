// Package api is the Control Plane's local API (ADR 0012): what `mamela api` serves on a Unix
// socket to mamelacd, and the client mamelacd uses. It offers exactly what a person does with the
// CLI on the Control Plane's host for gitops: plan a declaration of a commit's tree, approve a plan
// by its digest, ask whether a plan is approved, and apply an approved plan. Nothing else: no
// explicit operation (delete, reboot, adopt), no import, no state, no credential.
//
// Whoever can reach the socket can plan, approve and apply within the policy, the providers'
// guards and the catalogs, as the CLI can; the socket is therefore only for mamelacd's group.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/internal/tree"
)

// Version names the API. A client refuses a server of another version.
const Version = "mamela-api/v1"

// Info is what GET /v1/info says.
type Info struct {
	API    string `json:"api"`
	Mamela string `json:"mamela"`
}

// Planned is a saved plan, summarized for a pull request.
type Planned struct {
	Digest   string `json:"digest"`
	Revision string `json:"revision"` // the declaration's digest
	Changes  int    `json:"changes"`  // changes to make
	Waiting  int    `json:"waiting"`  // waiting for observation or dependencies
	Blocked  int    `json:"blocked"`  // refused (blocked or denied)
	Text     string `json:"text"`     // the plan as people read it, without the host's paths
	// Programs are the providers' programs the plan uses ("<provider> <version> (<source>, <digest>)").
	Programs []string `json:"programs,omitempty"`
}

// Applied is what applying did.
type Applied struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
	// Stale: the world was not what the plan assumed (MAM-REC-0002); nothing was changed for it.
	// The declaration is to be planned again.
	Stale bool `json:"stale,omitempty"`
}

// Backend does the work, exactly as the CLI does (cmd/mamela). dir is the declaration's directory
// in an extracted tree.
type Backend interface {
	Plan(ctx context.Context, dir string) (Planned, error)
	Revision(ctx context.Context, dir string) (string, error)
	Approve(ctx context.Context, digest, by string, ttl time.Duration) error
	Approved(ctx context.Context, digest string) (bool, error)
	Apply(ctx context.Context, digest string) (Applied, error)
}

// Requests.
type (
	TreeRef struct {
		Tree string `json:"tree"` // "sha256:..." of the tarball, from POST /v1/trees
		Dir  string `json:"dir"`  // the declaration's directory in the tree
	}
	ApproveRequest struct {
		Digest string `json:"digest"`
		By     string `json:"by"`  // "github:<login>"
		TTL    string `json:"ttl"` // a Go duration, at most MaxTTL
	}
	DigestRequest struct {
		Digest string `json:"digest"`
	}
)

// MaxTTL is the longest approval the API records.
const MaxTTL = 7 * 24 * time.Hour

// keepTrees is how many extracted trees the server keeps (the oldest go first).
const keepTrees = 32

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	byRe     = regexp.MustCompile(`^[a-z]+:[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
)

// Server serves the API. Requests run one at a time: a plan or an apply holds the others.
type Server struct {
	Backend Backend
	WorkDir string // extracted trees, one directory per tarball digest
	Mamela  string // the version, for /v1/info
	Logf    func(format string, args ...any)

	mu sync.Mutex
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		reply(w, Info{API: Version, Mamela: s.Mamela})
	})
	mux.HandleFunc("POST /v1/trees", s.locked(s.trees))
	mux.HandleFunc("POST /v1/plan", s.locked(s.plan))
	mux.HandleFunc("POST /v1/revision", s.locked(s.revision))
	mux.HandleFunc("POST /v1/approve", s.locked(s.approve))
	mux.HandleFunc("POST /v1/approved", s.locked(s.approved))
	mux.HandleFunc("POST /v1/apply", s.locked(s.apply))
	return mux
}

func (s *Server) locked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		h(w, r)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// trees extracts a tarball (internal/tree refuses links, paths that leave the tree and special
// files) and names it by its digest. The same tarball is extracted once.
func (s *Server) trees(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, tree.MaxTarball+1))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(data) > tree.MaxTarball {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the tarball is over %d bytes", tree.MaxTarball))
		return
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dir := filepath.Join(s.WorkDir, name)
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if err := tree.Extract(bytes.NewReader(data), dir); err != nil {
			fail(w, http.StatusUnprocessableEntity, fmt.Errorf("the tree is refused: %w", err))
			return
		}
		s.prune(name)
	}
	reply(w, TreeRef{Tree: "sha256:" + name})
}

// prune keeps the newest extracted trees (and the one just made).
func (s *Server) prune(keep string) {
	entries, err := os.ReadDir(s.WorkDir)
	if err != nil {
		return
	}
	type tr struct {
		name string
		mod  time.Time
	}
	var all []tr
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && len(e.Name()) == 64 {
			all = append(all, tr{e.Name(), info.ModTime()})
		}
	}
	slices.SortFunc(all, func(a, b tr) int { return b.mod.Compare(a.mod) })
	for i, t := range all {
		if i >= keepTrees && t.name != keep {
			if err := os.RemoveAll(filepath.Join(s.WorkDir, t.name)); err != nil {
				s.logf("api: removing the old tree %s: %v", t.name, err)
			}
		}
	}
}

// declaration resolves a tree and a directory in it.
func (s *Server) declaration(ref TreeRef) (string, error) {
	if !digestRe.MatchString(ref.Tree) {
		return "", fmt.Errorf("tree %q: sha256:<64 hex>", ref.Tree)
	}
	if !filepath.IsLocal(ref.Dir) || strings.Contains(ref.Dir, "\\") {
		return "", fmt.Errorf("dir %q: a directory in the tree", ref.Dir)
	}
	dir := filepath.Join(s.WorkDir, strings.TrimPrefix(ref.Tree, "sha256:"), filepath.FromSlash(ref.Dir))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("the tree %s has no directory %s (send the tree first)", ref.Tree, ref.Dir)
	}
	return dir, nil
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var ref TreeRef
	if !decode(w, r, &ref) {
		return
	}
	dir, err := s.declaration(ref)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Backend.Plan(r.Context(), dir)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.logf("api: planned %s of %s: %s", ref.Dir, ref.Tree, p.Digest)
	reply(w, p)
}

func (s *Server) revision(w http.ResponseWriter, r *http.Request) {
	var ref TreeRef
	if !decode(w, r, &ref) {
		return
	}
	dir, err := s.declaration(ref)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rev, err := s.Backend.Revision(r.Context(), dir)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	reply(w, DigestRequest{Digest: rev})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req ApproveRequest
	if !decode(w, r, &req) {
		return
	}
	ttl, err := time.ParseDuration(req.TTL)
	switch {
	case !digestRe.MatchString(req.Digest):
		err = fmt.Errorf("digest %q", req.Digest)
	case !byRe.MatchString(req.By):
		err = fmt.Errorf("by %q: <source>:<login>", req.By)
	case err != nil || ttl <= 0 || ttl > MaxTTL:
		err = fmt.Errorf("ttl %q: a duration up to %s", req.TTL, MaxTTL)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Backend.Approve(r.Context(), req.Digest, req.By, ttl); err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.logf("api: %s approved by %s for %s", req.Digest, req.By, ttl)
	reply(w, struct{}{})
}

func (s *Server) approved(w http.ResponseWriter, r *http.Request) {
	var req DigestRequest
	if !decode(w, r, &req) {
		return
	}
	if !digestRe.MatchString(req.Digest) {
		fail(w, http.StatusBadRequest, fmt.Errorf("digest %q", req.Digest))
		return
	}
	ok, err := s.Backend.Approved(r.Context(), req.Digest)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, struct {
		Approved bool `json:"approved"`
	}{ok})
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var req DigestRequest
	if !decode(w, r, &req) {
		return
	}
	if !digestRe.MatchString(req.Digest) {
		fail(w, http.StatusBadRequest, fmt.Errorf("digest %q", req.Digest))
		return
	}
	a, err := s.Backend.Apply(r.Context(), req.Digest)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.logf("api: applied %s: ok=%v", req.Digest, a.OK)
	reply(w, a)
}

const maxRequest = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequest))
	if err == nil {
		err = json.Unmarshal(data, v, json.RejectUnknownMembers(true))
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

type errorBody struct {
	Error string `json:"error"`
}

func reply(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func fail(w http.ResponseWriter, code int, err error) {
	data, _ := json.Marshal(errorBody{err.Error()})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}

// Listen listens on a Unix socket at path, readable and writable by its owner and group only. A
// stale socket left by a stopped server is replaced; any other file there is refused. group, when
// not empty, is the group given the socket (the server's user must be a member).
func Listen(path, group string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: another server is listening", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	if group != "" {
		if err := chgrp(path, group); err != nil {
			l.Close()
			return nil, err
		}
	}
	return l, nil
}

func chgrp(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, -1, gid)
}

// Client calls the API over its socket.
type Client struct {
	Socket string
	HTTP   *http.Client
}

// NewClient returns a client of the socket at path.
func NewClient(path string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	// No timeout of its own: an apply can take as long as creating a machine; the caller's
	// context bounds it.
	return &Client{Socket: path, HTTP: &http.Client{Transport: tr}}
}

func (c *Client) call(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://mamela"+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("the Control Plane's API (%s): %w", c.Socket, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &Error{Status: resp.StatusCode, Message: e.Error}
		}
		return &Error{Status: resp.StatusCode, Message: fmt.Sprintf("%.300s", data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Error is the API's refusal or failure.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Refused reports whether err is the API refusing what was asked (rather than not answering).
func Refused(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.call(ctx, "POST", path, "application/json", bytes.NewReader(data), out)
}

// Info checks that the server speaks this API.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	if err := c.call(ctx, "GET", "/v1/info", "", nil, &i); err != nil {
		return i, err
	}
	if i.API != Version {
		return i, fmt.Errorf("the Control Plane's API is %q, not %q: update mamela and mamelacd together", i.API, Version)
	}
	return i, nil
}

// Tree sends a commit's tarball; the answer names it for Plan and Revision.
func (c *Client) Tree(ctx context.Context, tarball []byte) (string, error) {
	var ref TreeRef
	err := c.call(ctx, "POST", "/v1/trees", "application/gzip", bytes.NewReader(tarball), &ref)
	return ref.Tree, err
}

// Plan plans the declaration dir of a tree and saves the plan.
func (c *Client) Plan(ctx context.Context, tree, dir string) (Planned, error) {
	var p Planned
	err := c.post(ctx, "/v1/plan", TreeRef{Tree: tree, Dir: dir}, &p)
	return p, err
}

// Revision evaluates the declaration dir of a tree: its digest.
func (c *Client) Revision(ctx context.Context, tree, dir string) (string, error) {
	var d DigestRequest
	err := c.post(ctx, "/v1/revision", TreeRef{Tree: tree, Dir: dir}, &d)
	return d.Digest, err
}

// Approve approves a saved plan by someone, for ttl.
func (c *Client) Approve(ctx context.Context, digest, by string, ttl time.Duration) error {
	return c.post(ctx, "/v1/approve", ApproveRequest{Digest: digest, By: by, TTL: ttl.String()}, nil)
}

// Approved says whether a plan has an approval that has not expired.
func (c *Client) Approved(ctx context.Context, digest string) (bool, error) {
	var out struct {
		Approved bool `json:"approved"`
	}
	err := c.post(ctx, "/v1/approved", DigestRequest{Digest: digest}, &out)
	return out.Approved, err
}

// Apply applies an approved plan.
func (c *Client) Apply(ctx context.Context, digest string) (Applied, error) {
	var a Applied
	err := c.post(ctx, "/v1/apply", DigestRequest{Digest: digest}, &a)
	return a, err
}
