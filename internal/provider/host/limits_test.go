package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// What a change makes or changes stays within the site's budget for one machine: the catalog says
// which parameters size it, and the host checks them, when planning and before sending, whatever
// was approved.
func TestLimitsOnMeasuredParameters(t *testing.T) {
	limits := map[string]int64{protocol.MeasureCPU: 2, protocol.MeasureMemoryMiB: 4096, protocol.MeasureDiskGiB: 20}
	create, _ := catalogs.Proxmox.Lookup("POST", "/nodes/{node}/qemu")
	resize, _ := catalogs.Proxmox.Lookup("PUT", "/nodes/{node}/qemu/{vmid}/resize")
	config, _ := catalogs.Proxmox.Lookup("PUT", "/nodes/{node}/qemu/{vmid}/config")
	for _, c := range []struct {
		name   string
		e      protocol.Entry
		params map[string][]string
		bad    string
	}{
		{"within", create, map[string][]string{"cores": {"2"}, "memory": {"4096"}, "scsi0": {"local-lvm:0,import-from=local:import/x.qcow2"}}, ""},
		{"too many vCPUs", create, map[string][]string{"cores": {"8"}}, "over the site's limit of 2"},
		{"too much memory", config, map[string][]string{"memory": {"65536"}}, "over the site's limit of 4096"},
		{"a big new disk", create, map[string][]string{"scsi0": {"local-lvm:500"}}, "over the site's limit of 20"},
		{"a disk that is not new", create, map[string][]string{"scsi0": {"local-lvm:vm-7000-disk-0"}}, "stated size"},
		{"grown too far", resize, map[string][]string{"size": {"1T"}}, "over the site's limit of 20"},
		{"grown by a relative size", resize, map[string][]string{"size": {"+5G"}}, "not an absolute size"},
		{"grown within", resize, map[string][]string{"size": {"20G"}}, ""},
		{"rounded up", resize, map[string][]string{"size": {"20481M"}}, "21 diskGiB"},
		{"not a number", config, map[string][]string{"cores": {"2,3"}}, "not a whole number"},
	} {
		err := c.e.CheckLimits(c.params, limits)
		if (c.bad == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.bad)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.bad)
		}
	}

	// The host refuses an over-limit request even when it was approved.
	m := protocol.Manifest{Schema: protocol.Schema, Provider: "p", Version: "1", API: "proxmox", Kinds: []protocol.Kind{{Kind: "VM",
		Operations: []protocol.Operation{{Op: "resize", Support: protocol.Supported, Calls: []protocol.Call{{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Form: []string{"cores", "digest"}}}}}}}}
	c := protocol.Change{Resource: "VM web", Kind: "VM", Op: "resize", IDs: map[string]string{"node": "n1", "vmid": "7"}, Precondition: "d1",
		Requests: []protocol.Approved{{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: map[string][]string{"cores": {"8"}, "digest": {"d1"}}}}}
	p, err := protocol.NewPermit(m, c, "op", "plan", "prov", testNow().Add(time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	api := &recorder{files: map[string]string{}}
	g := gate{phase: phaseApply, calls: p.Calls, permit: &p, catalog: catalogs.Proxmox, upstream: Handler{Handler: api}, now: testNow,
		used: make([]bool, 1), limits: limits}
	_, perr := g.send(context.Background(), protocol.HostRequest{Method: "PUT", Path: "/nodes/n1/qemu/7/config", Form: map[string][]string{"cores": {"8"}, "digest": {"d1"}}})
	if perr == nil || !strings.Contains(perr.Message, "over the site's limit") || len(api.got) != 0 {
		t.Fatalf("an approved request over the limit: %v, sent %v", perr, api.got)
	}
}
