package external

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/store"
)

// B10: a plan's error reaches a pull request. When the API cannot be reached, it says what kind of
// failure it was (MAM-PRV-0012) and not where the API is; where it is goes to the log of the
// Control Plane, which only its operator reads.
func TestAPIConnectionErrorsShowNoAddress(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close() // nothing is listening any more
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL + "/lab-internal/api", Catalog: "../../../examples/kv-demo/kv-catalog.json"}}}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(log.Writer())

	rt, err := Start(t.Context(), []store.Program{program(t, wasm(t, "../../../examples/kv/cmd/kv-provider"))}, cfg, wasmtime(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.SetCreations(noCreations{})
	_, err = rt.Observe(t.Context(), declaration(t), time.Unix(0, 0))
	if err == nil {
		t.Fatal("observe succeeded against an API that is not there")
	}
	t.Logf("the plan's error: %v", err)
	for _, secret := range []string{u.Host, u.Hostname(), u.Port(), "lab-internal"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the plan's error shows %q:\n%v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "MAM-PRV-0012") || !strings.Contains(err.Error(), "refused") {
		t.Errorf("the plan's error does not say what happened, with its code:\n%v", err)
	}
	if !strings.Contains(logged.String(), u.Host) || !strings.Contains(logged.String(), "provider kv-example") {
		t.Errorf("the operator's log lacks the detail:\n%s", logged.String())
	}
}
