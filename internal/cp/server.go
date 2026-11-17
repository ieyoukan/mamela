package cp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/store"
)

// Server serves mamelet.
type Server struct {
	Store *store.Store
	PKI   *PKI
	Now   func() time.Time
	// RevisionTTL is how long a served revision stays valid. While the Control Plane is
	// unreachable, mamelet keeps repairing drift from its last revision until it expires.
	RevisionTTL time.Duration
	Log         *log.Logger
	// MameletBinary is the mamelet a cloud machine's user-data seed fetches (ADR 0005): served at
	// /v1/mamelet/<its sha256> to anyone, since it is no secret; the seed checks the sha256.
	MameletBinary []byte
	// MameletFile, when set, is where MameletBinary comes from: it is read again when the file's size
	// or modification time changes, so a binary that is replaced is served without a restart (the
	// declaration pins the new sha256, and an update fetches it at once).
	MameletFile string
	// BodyTimeout is how long a request may take to deliver its body (zero: 10 seconds).
	BodyTimeout time.Duration

	mu    sync.Mutex
	sum   binarySum                 // the sha256 of MameletBinary
	file  fileStamp                 // the size and modification time MameletBinary was read at
	said  map[string]time.Time      // when a warning was last logged, by what it is about
	waits map[string][]*waitingNode // the requests waiting for a revision, by node, oldest first
}

// binarySum is the sha256 of a binary, kept with the slice it was computed from: the endpoint that
// serves the binary needs no login, so it must not hash it again for every request.
type binarySum struct {
	of  []byte
	hex string
}

const (
	// maxBody bounds what a mamelet may report.
	maxBody = 1 << 20
	// maxEnrollBody bounds an enrollment or renewal request: a JSON object with a certificate request.
	maxEnrollBody = 16 << 10
	// maxWaitsPerNode is how many requests of one node may wait for a revision at once; a further one
	// replaces the oldest. A mamelet has one, plus perhaps one from before its restart.
	maxWaitsPerNode = 2
	// warnEvery is how often the same warning is logged.
	warnEvery = 6 * time.Hour
	// CertWarnWithin is how soon before its end a mamelet's certificate is warned about: a mamelet
	// that can renew does so long before.
	CertWarnWithin = 30 * 24 * time.Hour
)

// fileStamp is what tells that a file was replaced, without reading it.
type fileStamp struct {
	size int64
	mod  time.Time
}

// binarySHA256 is the sha256 of MameletBinary, computed once for the slice the server has.
func (s *Server) binarySHA256() string {
	_, sum := s.binary()
	return sum
}

// binary is the mamelet the server serves and its sha256. With MameletFile, a file that was
// replaced (another size or modification time: a stat, not a read, per request) is read again.
func (s *Server) binary() ([]byte, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MameletFile != "" {
		if fi, err := os.Stat(s.MameletFile); err == nil && (fi.Size() != s.file.size || !fi.ModTime().Equal(s.file.mod)) {
			if b, err := os.ReadFile(s.MameletFile); err == nil {
				changed := s.file != (fileStamp{})
				s.MameletBinary, s.file = b, fileStamp{size: fi.Size(), mod: fi.ModTime()}
				if changed {
					s.logf("%s was replaced: serving the new mamelet", s.MameletFile)
				}
			}
		}
	}
	if len(s.MameletBinary) == 0 {
		return nil, ""
	}
	if len(s.sum.of) != len(s.MameletBinary) || &s.sum.of[0] != &s.MameletBinary[0] {
		sum := sha256.Sum256(s.MameletBinary)
		s.sum = binarySum{of: s.MameletBinary, hex: hex.EncodeToString(sum[:])}
	}
	return s.MameletBinary, s.sum.hex
}

// mamelet serves the mamelet binary, only under its own sha256.
func (s *Server) mamelet(w http.ResponseWriter, r *http.Request) {
	bin, want := s.binary()
	if want == "" || r.PathValue("sha256") != want {
		http.Error(w, "no such mamelet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(bin)
}

// TLSConfig requires TLS 1.3; client certificates are verified when given (enrollment has none).
// The certificate the server presents is looked up for every connection, so one that is about to end
// is replaced while the Control Plane runs (PKI.ServerCertificate).
func (s *Server) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.PKI.CA)
	base := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.PKI.Server},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool,
	}
	// GetConfigForClient, not GetCertificate: mamelet connects to an address, so there is no SNI, and
	// GetCertificate is not asked when Certificates is set.
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		cert, reissued, err := s.PKI.ServerCertificate(s.Now())
		if err != nil {
			s.warnf("server-cert", "MAM-PKI-0103: the server certificate is ending and could not be replaced: %v\n  hint: check that the Control Plane's user can write the directory of its keys (controlplane.state_dir)", err)
		} else if reissued {
			s.logf("MAM-PKI-0103: issued a new server certificate (valid until %s); the one before it was ending", cert.Leaf.NotAfter.Format("2006-01-02"))
		}
		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.Certificates = []tls.Certificate{*cert}
		cfg.NextProtos = []string{"http/1.1"}
		cfg.SessionTicketsDisabled = true // a resumed session would not look at the client certificate again
		return cfg, nil
	}
	return base
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	s.binarySHA256() // hash the binary now, not at the first request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", s.enroll)
	mux.HandleFunc("GET /v1/mamelet/{sha256}", s.mamelet)
	mux.HandleFunc("GET /v1/nodes/{node}/revision", s.revision)
	mux.HandleFunc("GET /v1/nodes/{node}/wait", s.wait)
	mux.HandleFunc("POST /v1/nodes/{node}/report", s.report)
	mux.HandleFunc("POST /v1/nodes/{node}/renew", s.renew)
	return mux
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

// warnf logs a warning at most once every warnEvery for what it is about (key), so that a mamelet
// that asks every thirty seconds does not fill the log with the same line.
func (s *Server) warnf(key, format string, args ...any) {
	now := s.Now()
	s.mu.Lock()
	if last, ok := s.said[key]; ok && now.Sub(last) < warnEvery {
		s.mu.Unlock()
		return
	}
	if s.said == nil {
		s.said = map[string]time.Time{}
	}
	s.said[key] = now
	s.mu.Unlock()
	s.logf(format, args...)
}

// HTTPErrorLog is the logger for the HTTP server's own errors, in particular a failed TLS
// handshake. When the cause is a client certificate that has ended, the line gets a code and what
// to do: that is the failure of a mamelet that was never renewed, which nothing else would explain
// (the request never reaches a handler).
func (s *Server) HTTPErrorLog() *log.Logger {
	if s.Log == nil {
		return nil
	}
	return log.New(handshakeExplainer{s}, "", 0)
}

type handshakeExplainer struct{ s *Server }

func (h handshakeExplainer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	h.s.logf("%s", line)
	if strings.Contains(line, "TLS handshake error") && strings.Contains(line, "certificate has expired") {
		from := handshakeSource(line)
		h.s.warnf("expired:"+from, "MAM-PKI-0102: a client at %s presented a certificate that has ended: a mamelet that was not renewed in time cannot connect\n"+
			"  hint: it must enroll again with a new one-time token: run `mamela controlplane agents` to see which node it is, then `mamela controlplane reenroll --node <name>` and follow what it prints (docs/recovery.md)", from)
	}
	return len(p), nil
}

// handshakeSource is the address a "TLS handshake error from <addr>: ..." line names.
func handshakeSource(line string) string {
	_, rest, ok := strings.Cut(line, "TLS handshake error from ")
	if !ok {
		return "an unknown address"
	}
	addr, _, _ := strings.Cut(rest, " ")
	addr = strings.TrimSuffix(addr, ":")
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func fail(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

// bodyTimeout is how long a request may take to deliver its body.
func (s *Server) bodyTimeout() time.Duration {
	if s.BodyTimeout > 0 {
		return s.BodyTimeout
	}
	return 10 * time.Second
}

// readJSON reads the request's body (at most limit bytes, and within the body timeout) into out.
// It answers the request itself when the body is too large or too slow, or does not decode.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	// The deadline is on the connection, not on the handler: a client that sends the headers and then
	// stops would otherwise keep the connection and its goroutine for as long as it likes.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyTimeout()))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	var timeout net.Error
	switch {
	case errors.As(err, &tooBig):
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("MAM-NODE-0202: the request is larger than %d bytes", limit))
		return false
	case errors.As(err, &timeout) && timeout.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		fail(w, http.StatusRequestTimeout, fmt.Sprintf("MAM-NODE-0202: the request body did not arrive within %s", s.bodyTimeout()))
		return false
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := json.Unmarshal(data, out, json.RejectUnknownMembers(true)); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req mamelet.EnrollRequest
	if !s.readJSON(w, r, maxEnrollBody, &req) {
		return
	}
	if !noderev.ValidNode(req.Node) {
		fail(w, http.StatusBadRequest, "invalid node name")
		return
	}
	// Nothing is signed for a caller without a good token (Enroll below decides for good, and spends it).
	if ok, err := s.Store.EnrollTokenUsable(r.Context(), req.Token, req.Node, req.VMID); err != nil {
		fail(w, http.StatusInternalServerError, "could not check the enrollment token")
		return
	} else if !ok {
		s.logf("enroll %s (VMID %d) refused: %v", req.Node, req.VMID, store.ErrEnrollRefused)
		fail(w, http.StatusForbidden, store.ErrEnrollRefused.Error())
		return
	}
	now := s.Now()
	certPEM, serialHex, err := s.PKI.SignAgent(req.CSR, req.Node, now)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// The token is checked and spent in the same transaction that records the certificate.
	if err := s.Store.Enroll(r.Context(), req.Token, req.Node, req.VMID, serialHex); err != nil {
		s.logf("enroll %s (VMID %d) refused: %v", req.Node, req.VMID, err)
		if errors.Is(err, store.ErrEnrollRefused) {
			fail(w, http.StatusForbidden, err.Error())
		} else {
			fail(w, http.StatusInternalServerError, "could not record the enrollment")
		}
		return
	}
	s.noteCert(r.Context(), req.Node, serialHex, certPEM)
	s.logf("enrolled %s (VMID %d), certificate %s", req.Node, req.VMID, serialHex)
	out, _ := json.Marshal(mamelet.EnrollResponse{Certificate: string(certPEM)})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// noteCert records when a newly issued certificate ends (best effort: it only feeds listings and
// warnings) and returns it.
func (s *Server) noteCert(ctx context.Context, node, registration string, certPEM []byte) (notAfter time.Time) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	if err := s.Store.NoteAgentCert(ctx, node, registration, cert.NotAfter); err != nil {
		s.logf("could not record the end of %s's certificate: %v", node, err)
	}
	return cert.NotAfter
}

// agentFor authenticates the request as the agent of the node in the path: a client certificate
// from our CA, naming that node, that stands for the registration recorded at its latest
// enrollment: the certificate issued then, or one that renewed it (see Registration).
func (s *Server) agentFor(w http.ResponseWriter, r *http.Request) (store.Agent, bool) {
	node := r.PathValue("node")
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		fail(w, http.StatusUnauthorized, "a client certificate is required")
		return store.Agent{}, false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	a, ok, err := s.Store.Agent(r.Context(), node)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "could not look up the agent")
		return a, false
	case leaf.Subject.CommonName != node || !ok || a.CertSerial != Registration(leaf):
		s.logf("refused %s %s: certificate %q is not the current one of node %s", r.Method, r.URL.Path, leaf.Subject.CommonName, node)
		fail(w, http.StatusForbidden, "this certificate is not the node's current one")
		return a, false
	}
	if left := leaf.NotAfter.Sub(s.Now()); left < CertWarnWithin {
		s.warnf("cert:"+node, "MAM-PKI-0101: the certificate of node %s ends %s (in %d days) and has not been renewed\n"+
			"  hint: a mamelet from 0.3.2 on renews it by itself from a Control Plane that supports it: update this node's mamelet (docs/mamelet-update.md); "+
			"if it ends before that, enroll the node again: mamela controlplane reenroll --node %s (docs/recovery.md)",
			node, leaf.NotAfter.Format("2006-01-02"), max(int(left.Hours()/24), 0), node)
	}
	return a, true
}

func (s *Server) revision(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	rev, err := s.Store.NodeRevision(r.Context(), a.Node)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the revision")
		return
	}
	if rev == nil || rev.VMID != a.VMID {
		fail(w, http.StatusNotFound, "no revision for this node yet")
		return
	}
	now := s.Now()
	ttl := s.RevisionTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	rev.IssuedAt, rev.ExpiresAt = now.UTC(), now.Add(ttl).UTC()
	// The validity period is renewed on every fetch, so it cannot end a grant: each grant has its own
	// deadline (set when it was issued), and one past it is not handed out.
	signed, err := noderev.Sign(s.PKI.RevisionKey, rev.ForMamelet(now))
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not sign the revision: "+err.Error())
		return
	}
	out, _ := json.Marshal(signed)
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// renew gives a mamelet a new certificate before its own ends, authenticated by that certificate:
// the node asks with a request signed by its key, the answer is a certificate for the same
// registration (see Registration). The database is not changed: what plans and approvals are bound to
// stays as it is, and a response that is lost does not lock the node out, because the old
// certificate keeps working until it ends. Only a certificate with less than half of its life left is
// renewed, so that a node cannot collect certificates.
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	leaf := r.TLS.VerifiedChains[0][0]
	now := s.Now()
	if leaf.NotAfter.Sub(now) > leaf.NotAfter.Sub(leaf.NotBefore)/2 {
		fail(w, http.StatusConflict, fmt.Sprintf("MAM-PKI-0105: the certificate ends %s; it is renewed once less than half of its life is left", leaf.NotAfter.Format("2006-01-02")))
		return
	}
	var req mamelet.RenewRequest
	if !s.readJSON(w, r, maxEnrollBody, &req) {
		return
	}
	certPEM, serialHex, err := s.PKI.SignRenewal(req.CSR, a.Node, a.CertSerial, now)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	notAfter := s.noteCert(r.Context(), a.Node, a.CertSerial, certPEM)
	s.logf("renewed the certificate of %s: %s, valid until %s", a.Node, serialHex, notAfter.Format("2006-01-02"))
	out, _ := json.Marshal(mamelet.EnrollResponse{Certificate: string(certPEM)})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// MaxWait bounds how long a request for a newer revision waits.
const MaxWait = 30 * time.Second

// waitPoll is how often a waiting request looks for a newer revision.
const waitPoll = time.Second

// waitingNode is a request that waits for a revision; replaced is closed when a newer request of
// the same node pushed it out.
type waitingNode struct{ replaced chan struct{} }

// enterWait registers a waiting request of node. If the node already has maxWaitsPerNode, the oldest
// is told to end. The returned function removes the request.
func (s *Server) enterWait(node string) (*waitingNode, func()) {
	w := &waitingNode{replaced: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waits == nil {
		s.waits = map[string][]*waitingNode{}
	}
	s.waits[node] = append(s.waits[node], w)
	for len(s.waits[node]) > maxWaitsPerNode {
		close(s.waits[node][0].replaced)
		s.waits[node] = s.waits[node][1:]
	}
	return w, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		kept := s.waits[node][:0:0]
		for _, o := range s.waits[node] {
			if o != w {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(s.waits, node)
		} else {
			s.waits[node] = kept
		}
	}
}

// wait answers when the node's newest revision is newer than ?after=<serial>, or after
// ?seconds=<n> (at most MaxWait): {"serial": N, "changed": bool}. It lets mamelet act on a grant at
// once instead of at its next poll. It reveals nothing a revision request does not (the node's own
// certificate is required) and changes nothing. A node may have two such requests waiting; a third
// ends the oldest (409), so that one node cannot hold any number of connections and database polls.
func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	after, err1 := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	secs, err2 := strconv.Atoi(r.URL.Query().Get("seconds"))
	if err1 != nil || err2 != nil || after < 0 || secs < 1 {
		fail(w, http.StatusBadRequest, "after (a serial) and seconds (1 or more) are required")
		return
	}
	me, leave := s.enterWait(a.Node)
	defer leave()
	deadline := time.NewTimer(min(time.Duration(secs)*time.Second, MaxWait))
	defer deadline.Stop()
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		serial, err := s.Store.NodeRevisionSerial(r.Context(), a.Node)
		if err != nil {
			fail(w, http.StatusInternalServerError, "could not read the revision")
			return
		}
		if serial > after {
			s.waited(w, serial, true)
			return
		}
		if after > serial {
			s.staleDatabase(a.Node, after, serial)
		}
		select {
		case <-r.Context().Done():
			return
		case <-me.replaced:
			fail(w, http.StatusConflict, fmt.Sprintf("MAM-NODE-0201: node %s has more than %d requests waiting; a newer one replaced this", a.Node, maxWaitsPerNode))
			return
		case <-deadline.C:
			s.waited(w, serial, false)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) waited(w http.ResponseWriter, serial int64, changed bool) {
	out, _ := json.Marshal(struct {
		Serial  int64 `json:"serial"`
		Changed bool  `json:"changed"`
	}{serial, changed})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var rep noderev.Report
	if !s.readJSON(w, r, maxBody, &rep) {
		return
	}
	if err := checkReport(rep, a); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	prev, _, _ := s.Store.NodeReportOf(r.Context(), a.Node)
	if err := s.Store.PutAuthenticatedNodeReport(r.Context(), rep, a.CertSerial); err != nil {
		if errors.Is(err, store.ErrAgentRevoked) {
			fail(w, http.StatusForbidden, "agent registration changed")
			return
		}
		fail(w, http.StatusInternalServerError, "could not store the report")
		return
	}
	s.checkNotNewer(r.Context(), rep)
	for _, res := range rep.Results {
		if res.Operation == "repair" {
			s.logf("%s: %s: %s", a.Node, res.Outcome, res.Detail)
		}
	}
	// A node that does not accept the revision it is offered can be given no permission, and nothing
	// else says so (the plan does, from the report): log when that starts, changes and ends, not at
	// each report.
	switch {
	case rep.Refused != "" && rep.Refused != prev.Report.Refused:
		s.logf("%s: MAM-PLAN-0130: mamelet refuses the revision (it works from %d): %q", a.Node, rep.Serial, rep.Refused)
	case rep.Refused == "" && prev.Report.Refused != "":
		s.logf("%s: mamelet accepts the Control Plane's revision again (revision %d)", a.Node, rep.Serial)
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkNotNewer warns when a mamelet works from a revision newer than the newest the database has,
// or has refused one as older than what it accepted: the database is older than the mamelet, which
// happens when it was restored from an old backup.
func (s *Server) checkNotNewer(ctx context.Context, rep noderev.Report) {
	have, err := s.Store.NodeRevisionSerial(ctx, rep.Node)
	if err != nil {
		return
	}
	if rep.Serial > have || strings.Contains(rep.Refused, noderev.ErrRolledBack.Error()) {
		s.staleDatabase(rep.Node, rep.Serial, have)
	}
}

func (s *Server) staleDatabase(node string, mameletSerial, databaseSerial int64) {
	s.warnf("stale:"+node, "MAM-BAK-0101: node %s has accepted revision %d, but this database has only %d for it: the database is older than the mamelet "+
		"(restored from an old backup?), and the mamelet refuses revisions with a lower serial\n"+
		"  hint: restore a newer backup; if there is none, the mamelet must forget its serial (docs/recovery.md, \"A database older than the nodes\")",
		node, mameletSerial, databaseSerial)
}

// checkReport rejects reports that are not about the agent's node or that do not hold together;
// a report is an observation, never trusted as sent.
func checkReport(rep noderev.Report, a store.Agent) error {
	if rep.Node != a.Node || rep.VMID != a.VMID {
		return fmt.Errorf("the report is for %s (VMID %d), the certificate for %s (VMID %d)", rep.Node, rep.VMID, a.Node, a.VMID)
	}
	for _, p := range rep.Packages {
		if !noderev.ValidPackage(p.Name) {
			return fmt.Errorf("invalid package name %q", p.Name)
		}
		if p.ChangeSet != nil {
			if err := p.ChangeSet.Check(); err != nil {
				return fmt.Errorf("package %s: %w", p.Name, err)
			}
		}
	}
	for _, sv := range rep.Services {
		if !noderev.ValidService(sv.Name) {
			return fmt.Errorf("invalid service name %q", sv.Name)
		}
	}
	// The node extensions' members (noderev's ext.go): names of extensions, versions from 1, and
	// observations that are JSON objects. What is in an observation is its extension's to read.
	for _, name := range slices.Sorted(maps.Keys(rep.Extensions)) {
		if !names.Extension(name) || rep.Extensions[name] < 1 {
			return fmt.Errorf("extensions: %q %d is not an extension and a version", name, rep.Extensions[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rep.Ext)) {
		if p := rep.Ext[name]; !names.Extension(name) || !p.IsValid() || p.Kind() != '{' {
			return fmt.Errorf("ext %q is not an extension's observation (a JSON object)", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rep.ExtRefused)) {
		if !names.Extension(name) {
			return fmt.Errorf("extRefused %q is not an extension", name)
		}
	}
	return nil
}
