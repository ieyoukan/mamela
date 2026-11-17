package cp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// syncBuf is a log destination the server's goroutines and the test may use together.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logTo makes the Control Plane log into a buffer the test can read.
func (e *env) logTo() *syncBuf {
	buf := &syncBuf{}
	e.cp.Log = log.New(buf, "", 0)
	return buf
}

// caPool is a pool that holds the Control Plane's CA.
func (e *env) caPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(e.pki.CA)
	return pool
}

// A request for the mamelet binary under a sha256 that is not the binary's is answered from the
// path alone: it must not cost a hash of the whole binary each time (the endpoint needs no login,
// so anyone could otherwise keep the Control Plane busy hashing).
func TestMameletEndpointDoesNotHashPerRequest(t *testing.T) {
	e := newEnv(t)
	bin := make([]byte, 48<<20)
	s := &Server{Store: e.st, PKI: e.pki, Now: func() time.Time { return e.now }, MameletBinary: bin}
	h := s.Handler()
	start := time.Now()
	sum := sha256.Sum256(bin)
	oneHash := time.Since(start)
	_ = sum
	ask := func(sha string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/mamelet/"+sha, nil))
		return rec.Code
	}
	ask(strings.Repeat("0", 64)) // whatever one-time work the server does, it is done by now
	start = time.Now()
	for range 50 {
		if code := ask(strings.Repeat("0", 64)); code != 404 {
			t.Fatalf("a wrong sha256: %d", code)
		}
	}
	if took := time.Since(start); took > oneHash/2 {
		t.Fatalf("50 requests for a sha256 that is not served took %v; hashing the binary once takes %v: the server hashes it per request", took, oneHash)
	}
}

// Enrollment checks the token before it signs anything with the CA: a caller without a good token
// gets nothing signed, whatever it sends.
func TestEnrollChecksTheTokenBeforeSigning(t *testing.T) {
	e := newEnv(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.caPool()}}}
	resp, err := client.Post(e.srv.URL+"/v1/enroll", "application/json",
		strings.NewReader(`{"node":"web","vmid":7001,"token":"not-a-token","csr":"not a request"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unknown token with a bad request: %s (the request was examined, and so possibly signed, before the token was checked)", resp.Status)
	}
}

// A body that arrives too slowly is cut off; it does not hold a connection (and a goroutine) open.
func TestSlowEnrollmentBodyIsCutOff(t *testing.T) {
	e := newEnv(t)
	e.cp.BodyTimeout = 300 * time.Millisecond
	conn, err := tls.Dial("tcp", e.srv.Listener.Addr().String(), &tls.Config{RootCAs: e.caPool(), ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The header promises 500 bytes; one arrives.
	if _, err := conn.Write([]byte("POST /v1/enroll HTTP/1.1\r\nHost: cp\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("the server did not answer a body that stopped arriving within 3s: %v", err)
	}
	if !strings.Contains(line, "408") {
		t.Fatalf("answer %q, want 408 Request Timeout", line)
	}
}

// An oversized enrollment body is refused without being read to the end.
func TestEnrollmentBodyHasASmallLimit(t *testing.T) {
	e := newEnv(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.caPool()}}}
	big := `{"node":"web","vmid":7001,"token":"x","csr":"` + strings.Repeat("A", 200<<10) + `"}`
	resp, err := client.Post(e.srv.URL+"/v1/enroll", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 200 KiB enrollment request: %s", resp.Status)
	}
}

// A node may keep at most two requests waiting for a revision: a further one replaces the oldest,
// which is answered and closed. Other nodes are not affected.
func TestWaitsOfOneNodeAreLimited(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx) // ends the waits that are still open when the test is done
	defer cancel()
	web, db := e.enroll("web", 7001), e.enroll("db", 7002)
	for node, vmid := range map[string]int{"web": 7001, "db": 7002} {
		e.st.PublishNodeRevision(e.ctx, node, func(*noderev.Revision) (noderev.Revision, error) {
			return noderev.Revision{VMID: vmid}, nil
		})
	}
	type result struct {
		changed bool
		err     error
		after   time.Duration
	}
	// registered is how many requests of node the server has waiting.
	registered := func(node string) int {
		e.cp.mu.Lock()
		defer e.cp.mu.Unlock()
		return len(e.cp.waits[node])
	}
	wait := func(cfg mamelet.Config, want int) chan result {
		out := make(chan result, 1)
		c, err := mamelet.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		go func() {
			_, changed, err := c.WaitChange(ctx, 1, 20*time.Second)
			out <- result{changed, err, time.Since(start)}
		}()
		for deadline := time.Now().Add(5 * time.Second); registered(cfg.Node) != want; {
			if time.Now().After(deadline) {
				t.Fatalf("the server has %d waits of %s, want %d", registered(cfg.Node), cfg.Node, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return out
	}
	other := wait(db, 1)
	first := wait(web, 1)
	second := wait(web, 2)
	select {
	case r := <-first:
		t.Fatalf("the first wait ended before the limit was reached: %+v", r)
	default:
	}
	third := wait(web, 2) // the third replaces the first: two are waiting still
	select {
	case r := <-first:
		if r.after > 5*time.Second {
			t.Fatalf("the oldest wait was replaced only after %v", r.after)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a third wait of the same node did not replace the oldest (three are open)")
	}
	for name, ch := range map[string]chan result{"second": second, "third": third, "other node": other} {
		select {
		case r := <-ch:
			t.Fatalf("the %s wait was ended: %+v", name, r)
		default:
		}
	}
}

// The server certificate is renewed while the Control Plane runs: a Control Plane that is never
// restarted would otherwise serve an expired certificate after a year.
func TestServerCertificateIsReissuedWhileRunning(t *testing.T) {
	e := newEnv(t)
	logs := e.logTo()
	at := func(t0 time.Time) *x509.Certificate {
		t.Helper()
		e.now = t0
		conn, err := tls.Dial("tcp", e.srv.Listener.Addr().String(),
			&tls.Config{RootCAs: e.caPool(), ServerName: "127.0.0.1", Time: func() time.Time { return t0 }})
		if err != nil {
			t.Fatalf("a client whose clock says %s cannot connect: %v", t0.Format("2006-01-02"), err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}
	first := at(e.now)
	if again := at(e.now.AddDate(0, 6, 0)); !again.Equal(first) {
		t.Fatal("the certificate was replaced six months in, long before it expires")
	}
	late := e.now.AddDate(0, 0, 366)
	renewed := at(late)
	if !renewed.NotAfter.After(late.AddDate(0, 6, 0)) {
		t.Fatalf("a year and a day in, the server presents a certificate that ends %s", renewed.NotAfter)
	}
	if !strings.Contains(logs.String(), "MAM-PKI-0103") {
		t.Fatalf("the reissue is not logged:\n%s", logs.String())
	}
	// It is the file a restart would load, too.
	if reloaded, err := LoadPKI(e.dir, []string{"127.0.0.1"}, late); err != nil || !reloaded.Server.Leaf.Equal(renewed) {
		t.Fatalf("the reissued certificate was not saved: %v", err)
	}
}

// A mamelet whose certificate has expired cannot even finish the handshake; the Control Plane's
// log says so with a code and what to do, not only "TLS handshake error".
func TestExpiredClientCertificateIsExplainedInTheLog(t *testing.T) {
	e := newEnv(t)
	logs := e.logTo()
	e.srv.Config.ErrorLog = e.cp.HTTPErrorLog()
	real := e.now
	e.now = real.AddDate(0, 0, -366) // enrolled a year and a day ago: its certificate ended yesterday
	cfg := e.enroll("old", 7003)
	e.now = real
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Revision(e.ctx); err == nil || !strings.Contains(err.Error(), "expired certificate") {
		t.Fatalf("the expired certificate was accepted or failed otherwise: %v", err)
	}
	// The server logs the failed handshake on its own goroutine, possibly after the client has
	// already seen the alert: wait a little for the line.
	var out string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		out = logs.String()
		if strings.Contains(out, "MAM-PKI-0102") || time.Now().After(deadline) {
			break
		}
	}
	if !strings.Contains(out, "MAM-PKI-0102") || !strings.Contains(out, "reenroll") {
		t.Fatalf("the handshake failure is not explained:\n%s", out)
	}
}

// A certificate that is about to expire is named in the log by the node it belongs to, with what
// to do: the mamelet could not renew it (an older mamelet, or the renewal fails).
func TestCertificateNearExpiryIsWarnedAbout(t *testing.T) {
	e := newEnv(t)
	logs := e.logTo()
	real := e.now
	e.now = real.AddDate(0, 0, -350) // 15 days left
	cfg := e.enroll("web", 7001)
	e.now = real
	e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) { return noderev.Revision{VMID: 7001}, nil })
	c, _ := mamelet.NewClient(cfg)
	if _, err := c.Revision(e.ctx); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "MAM-PKI-0101") || !strings.Contains(out, "web") || !strings.Contains(out, "hint:") {
		t.Fatalf("no warning:\n%s", out)
	}
	// Once per node and while: the next request does not log it again.
	before := strings.Count(out, "MAM-PKI-0101")
	if _, err := c.Revision(e.ctx); err != nil {
		t.Fatal(err)
	}
	if after := strings.Count(logs.String(), "MAM-PKI-0101"); after != before {
		t.Fatalf("the warning is logged on every request (%d, then %d)", before, after)
	}
}

// A database older than the mamelets' last accepted revisions (restored from an old backup) is
// noticed from their reports: mamelet refuses any revision with a lower serial.
func TestDatabaseOlderThanAMameletIsWarnedAbout(t *testing.T) {
	e := newEnv(t)
	logs := e.logTo()
	cfg := e.enroll("web", 7001)
	e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) { return noderev.Revision{VMID: 7001}, nil })
	c, _ := mamelet.NewClient(cfg)
	if err := c.Report(e.ctx, noderev.Report{Node: "web", VMID: 7001, Serial: 1}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "MAM-BAK-0101") {
		t.Fatalf("warned although the database is current:\n%s", logs.String())
	}
	if err := c.Report(e.ctx, noderev.Report{Node: "web", VMID: 7001, Serial: 9}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "MAM-BAK-0101") || !strings.Contains(out, "web") || !strings.Contains(out, "hint:") {
		t.Fatalf("no warning:\n%s", out)
	}
	// A mamelet that refused the revision and has no cached one says so in the report.
	dbCfg := e.enroll("db", 7002)
	e.st.PublishNodeRevision(e.ctx, "db", func(*noderev.Revision) (noderev.Revision, error) { return noderev.Revision{VMID: 7002}, nil })
	dc, _ := mamelet.NewClient(dbCfg)
	logs2 := e.logTo()
	rolled := noderev.Report{Node: "db", VMID: 7002, Refused: noderev.ErrRolledBack.Error() + ": serial 1 < 9"}
	if err := dc.Report(e.ctx, rolled); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs2.String(), "MAM-BAK-0101") {
		t.Fatalf("a mamelet that refused a rolled-back revision is not noticed:\n%s", logs2.String())
	}
}
