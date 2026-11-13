package cp

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
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
}

const maxBody = 1 << 20

// mamelet serves the mamelet binary, only under its own sha256.
func (s *Server) mamelet(w http.ResponseWriter, r *http.Request) {
	sum := sha256.Sum256(s.MameletBinary)
	if len(s.MameletBinary) == 0 || r.PathValue("sha256") != hex.EncodeToString(sum[:]) {
		http.Error(w, "no such mamelet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(s.MameletBinary)
}

// TLSConfig requires TLS 1.3; client certificates are verified when given (enrollment has none).
func (s *Server) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.PKI.CA)
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.PKI.Server},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool,
	}
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", s.enroll)
	mux.HandleFunc("GET /v1/mamelet/{sha256}", s.mamelet)
	mux.HandleFunc("GET /v1/nodes/{node}/revision", s.revision)
	mux.HandleFunc("POST /v1/nodes/{node}/report", s.report)
	return mux
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func fail(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

func readJSON(r *http.Request, out any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxBody {
		return errors.New("request too large")
	}
	return json.Unmarshal(data, out, json.RejectUnknownMembers(true))
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req mamelet.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !noderev.ValidNode(req.Node) {
		fail(w, http.StatusBadRequest, "invalid node name")
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
	s.logf("enrolled %s (VMID %d), certificate %s", req.Node, req.VMID, serialHex)
	out, _ := json.Marshal(mamelet.EnrollResponse{Certificate: string(certPEM)})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// agentFor authenticates the request as the agent of the node in the path: a client certificate
// from our CA, naming that node, whose serial is the one recorded at its latest enrollment.
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
	case leaf.Subject.CommonName != node || !ok || a.CertSerial != hex.EncodeToString(leaf.SerialNumber.Bytes()):
		s.logf("refused %s %s: certificate %q is not the current one of node %s", r.Method, r.URL.Path, leaf.Subject.CommonName, node)
		fail(w, http.StatusForbidden, "this certificate is not the node's current one")
		return a, false
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
	signed, err := noderev.Sign(s.PKI.RevisionKey, *rev)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not sign the revision: "+err.Error())
		return
	}
	out, _ := json.Marshal(signed)
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var rep noderev.Report
	if err := readJSON(r, &rep); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkReport(rep, a); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.PutAuthenticatedNodeReport(r.Context(), rep, a.CertSerial); err != nil {
		if errors.Is(err, store.ErrAgentRevoked) {
			fail(w, http.StatusForbidden, "agent registration changed")
			return
		}
		fail(w, http.StatusInternalServerError, "could not store the report")
		return
	}
	for _, res := range rep.Results {
		if res.Operation == "repair" {
			s.logf("%s: %s: %s", a.Node, res.Outcome, res.Detail)
		}
	}
	w.WriteHeader(http.StatusNoContent)
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
	return nil
}
