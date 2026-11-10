package proxmox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRestrictSendsOnlyDeclaredCalls(t *testing.T) {
	sent := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Write([]byte(`{"data":null}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL + "/api2/json", http: srv.Client(), timeout: 5e9, maxBody: 1 << 20}
	ctx := Restrict(context.Background(), "resize", []Call{
		{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: []string{"cores", "memory", "digest"}},
	})

	if err := c.SetSize(ctx, "n", 7000, 2, 2048, "d"); err != nil {
		t.Fatalf("declared call refused: %v", err)
	}
	for name, call := range map[string]func() error{
		"undeclared call":      func() error { _, err := c.RebootVM(ctx, "n", 7000); return err },
		"undeclared parameter": func() error { return c.SetMarker(ctx, "n", 7000, "x", "y", "") },
		"narrowed": func() error {
			return c.SetSize(Restrict(ctx, "observe", []Call{{Method: "GET", Path: "/cluster/resources"}}), "n", 7000, 1, 1, "d")
		},
	} {
		err := call()
		if !errors.Is(err, ErrNotSent) || Uncertain(err) {
			t.Errorf("%s: %v (want a refusal before sending, with a known outcome)", name, err)
		}
	}
	if sent != 1 {
		t.Errorf("%d requests sent, want 1", sent)
	}
}
