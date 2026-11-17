package cp

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A mamelet binary that is replaced on disk is served without restarting the Control Plane: the
// declaration pins the new sha256, and the update fetches it at once (found on the lab: the update
// failed with "does not serve" until mamela serve was restarted).
func TestReplacedMameletFileIsServed(t *testing.T) {
	e := newEnv(t)
	file := filepath.Join(t.TempDir(), "mamelet-linux-amd64")
	old, new := []byte("mamelet 0.9.1"), []byte("mamelet 0.9.2, a little longer")
	if err := os.WriteFile(file, old, 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	s := &Server{Store: e.st, PKI: e.pki, Now: func() time.Time { return e.now }, MameletFile: file}
	s.Log = log.New(logs, "", 0)
	h := s.Handler()
	ask := func(b []byte) (int, string) {
		sum := sha256.Sum256(b)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/mamelet/"+hex.EncodeToString(sum[:]), nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := ask(old); code != 200 || body != string(old) {
		t.Fatalf("the first binary: %d %q", code, body)
	}
	if err := os.WriteFile(file, new, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := ask(new); code != 200 || body != string(new) {
		t.Fatalf("the replaced binary is not served: %d %q", code, body)
	}
	if code, _ := ask(old); code != 404 {
		t.Fatalf("the old binary is still served: %d", code)
	}
	if !strings.Contains(logs.String(), "was replaced") {
		t.Errorf("the replacement is not logged: %s", logs.String())
	}
}
