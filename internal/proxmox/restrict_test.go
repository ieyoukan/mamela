package proxmox

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func testClient(t *testing.T) (*Client, *int) {
	sent := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Write([]byte(`{"data":null}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/api2/json", http: srv.Client(), timeout: 5e9, maxBody: 1 << 20}, &sent
}

var resizeCalls = []Call{
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"},
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: []string{"cores", "memory", "digest"}},
}

func refused(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotSent) || Uncertain(err) {
		t.Errorf("%s: %v (want a refusal before sending, with a known outcome)", name, err)
	}
}

func TestRestrictSendsOnlyDeclaredCalls(t *testing.T) {
	c, sent := testClient(t)
	ctx := Restrict(context.Background(), "resize", resizeCalls)
	if err := c.SetSize(ctx, "n", 7000, 2, 2048, "d"); err != nil {
		t.Fatalf("declared call refused: %v", err)
	}
	_, err := c.RebootVM(ctx, "n", 7000)
	refused(t, "undeclared call", err)
	refused(t, "undeclared parameter", c.SetMarker(ctx, "n", 7000, "x", "y", ""))
	refused(t, "narrowed", c.SetSize(Restrict(ctx, "observe", []Call{{Method: "GET", Path: "/cluster/resources"}}), "n", 7000, 1, 1, "d"))
	if *sent != 1 {
		t.Errorf("%d requests sent, want 1", *sent)
	}
}

// An operation changes only the objects it is bound to, with the approved values; reads are not
// limited by the binding.
func TestOperationIsBoundToTheApprovedObjectAndValues(t *testing.T) {
	c, sent := testClient(t)
	ctx := RestrictOperation(context.Background(), "resize-vm-cpu", resizeCalls)
	ctx = Bind(ctx, map[string]string{"node": "n"})
	ctx = Values(ctx, "PUT", "/nodes/{node}/qemu/{vmid}/config", url.Values{"cores": {"2"}, "memory": {"2048"}})

	refused(t, "VMID not bound yet", c.SetSize(ctx, "n", 7000, 2, 2048, "d"))
	if _, err := c.VMStatus(ctx, "n", 7001); err != nil {
		t.Errorf("a read before binding: %v", err)
	}
	bound := Bind(ctx, map[string]string{"vmid": "7000"})
	refused(t, "another VM", c.SetSize(bound, "n", 7001, 2, 2048, "d"))
	refused(t, "another node", c.SetSize(bound, "m", 7000, 2, 2048, "d"))
	refused(t, "more vCPUs than approved", c.SetSize(bound, "n", 7000, 8, 2048, "d"))
	refused(t, "more memory than approved", c.SetSize(bound, "n", 7000, 2, 65536, "d"))
	refused(t, "bound twice to different VMs", c.SetSize(Bind(bound, map[string]string{"vmid": "7001"}), "n", 7000, 2, 2048, "d"))
	if got := Changed(bound); len(got) != 0 {
		t.Errorf("changed before anything was sent: %v", got)
	}
	if err := c.SetSize(bound, "n", 7000, 2, 2048, "d"); err != nil {
		t.Fatalf("the approved change: %v", err)
	}
	if got := Changed(bound); !slices.Equal(got, []string{"PUT /nodes/n/qemu/7000/config"}) {
		t.Errorf("changed: %v", got)
	}
	if *sent != 2 {
		t.Errorf("%d requests sent, want 2 (one read, one change)", *sent)
	}
}

func TestReadOnlySendsNoChange(t *testing.T) {
	c, sent := testClient(t)
	ctx := ReadOnly(context.Background(), "observe")
	refused(t, "a change while observing", c.SetSize(ctx, "n", 7000, 1, 1, "d"))
	if _, err := c.StartVM(ctx, "n", 7000); !errors.Is(err, ErrNotSent) {
		t.Errorf("start while observing: %v", err)
	}
	if _, err := c.VMStatus(ctx, "n", 7000); err != nil {
		t.Errorf("a read while observing: %v", err)
	}
	if *sent != 1 {
		t.Errorf("%d requests sent, want 1", *sent)
	}
}

// A call declared without Params sends no parameter: a forgotten declaration refuses, it never
// allows everything.
func TestUndeclaredParamsAreNoParams(t *testing.T) {
	c, sent := testClient(t)
	ctx := Restrict(context.Background(), "resize", []Call{{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config"}})
	refused(t, "config without declared params", c.SetSize(ctx, "n", 7000, 2, 2048, "d"))
	ctx = Restrict(context.Background(), "start", []Call{{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start"}})
	if _, err := c.StartVM(ctx, "n", 7000); err != nil {
		t.Errorf("a call that sends no parameter: %v", err)
	}
	if *sent != 1 {
		t.Errorf("%d requests sent, want 1", *sent)
	}
}

// Multipart fields and query parameters are checked like form parameters.
func TestMultipartAndQueryParamsAreChecked(t *testing.T) {
	c, sent := testClient(t)
	upload := func(params ...string) error {
		ctx := Restrict(context.Background(), "upload", []Call{{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Params: params}})
		_, err := c.Upload(ctx, "n", "local", "iso", "seed.iso", "abc", bytes.NewReader([]byte("x")), 1)
		return err
	}
	refused(t, "multipart with only content declared", upload("content"))
	if err := upload("content", "checksum-algorithm", "checksum", "filename"); err != nil {
		t.Errorf("multipart with every field declared: %v", err)
	}
	next := func(params ...string) error {
		_, err := c.VMIDFree(Restrict(context.Background(), "observe", []Call{{Method: "GET", Path: "/cluster/nextid", Params: params}}), 7000)
		return err
	}
	refused(t, "query parameter not declared", next())
	if err := next("vmid"); err != nil {
		t.Errorf("declared query parameter: %v", err)
	}
	delete := func(params ...string) error {
		ctx := Restrict(context.Background(), "destroy", []Call{{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Params: params}})
		_, err := c.WithDestroy().DestroyVM(ctx, "n", 7000)
		return err
	}
	refused(t, "destroy query not declared", delete())
	if err := delete("destroy-unreferenced-disks"); err != nil {
		t.Errorf("declared destroy query: %v", err)
	}
	if *sent != 3 {
		t.Errorf("%d requests sent, want 3", *sent)
	}
}
