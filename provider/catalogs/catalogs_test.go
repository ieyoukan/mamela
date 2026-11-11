package catalogs

import (
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The catalogs Mamela ships are consistent, and an EC2 instance's size is read from its type.
func TestCatalogs(t *testing.T) {
	for _, c := range []protocol.Catalog{Proxmox, EC2} {
		if err := c.Check(); err != nil {
			t.Fatal(err)
		}
	}
	run, ok := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {EC2Version}, "MinCount": {"1"}, "MaxCount": {"1"}}})
	if !ok || run.Effect != protocol.EffectCreate {
		t.Fatal("RunInstances")
	}
	limits := map[string]int64{protocol.MeasureCPU: 2, protocol.MeasureMemoryMiB: 4096, protocol.MeasureDiskGiB: 20}
	for typ, ok := range map[string]bool{"t3.medium": true, "t3.large": false, "m6i.xlarge": false, "x9.huge": false} {
		if err := run.CheckLimits(map[string][]string{"InstanceType": {typ}}, limits); (err == nil) != ok {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, ok := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {EC2Version}, "MinCount": {"1"}, "MaxCount": {"10"}}}); ok {
		t.Fatal("a RunInstances of ten instances is a known call")
	}
	if term, _ := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"TerminateInstances"}, "Version": {EC2Version}}}); term.Effect != protocol.EffectLoseData {
		t.Fatal("TerminateInstances loses data")
	}
}
