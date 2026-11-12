// Package ec2provider is an example Mamela provider of Amazon EC2 instances that mamelet runs on
// (ADR 0005). It runs isolated (WASM) and reaches EC2 only through the Mamela host, which signs
// each request (SigV4), checks it against Mamela's EC2 catalog and the change's permit, and fills
// in the instance's user-data: a mamelet seed with a one-time enrollment token, which this program
// never sees.
//
// Kind Ec2Instance is a node kind. The provider launches instances (RunInstances, one at a time,
// with a client token so that a retry never launches a second) and starts stopped ones. It never
// changes an instance's type or image, and never terminates one.
package ec2provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// API, kind, operations.
const (
	API                           = "ec2"
	Version                       = "0.1.0"
	Kind    protocol.ResourceKind = "Ec2Instance"

	OpCreate       protocol.Op = "create-vm"
	OpStart        protocol.Op = "start-vm"
	OpChangeType   protocol.Op = "change-instance-type"
	OpReplaceImage protocol.Op = "replace-vm-image"
	OpDeleteVM     protocol.Op = "delete-vm"
	ec2APIVersion              = "2016-11-15"
)

// Tags that mark Mamela's instances.
const (
	tagManaged = "mamela-managed"
	tagName    = "mamela-name"
	tagNode    = "mamela-node"
)

// Spec is a declared instance.
type Spec struct {
	ImageID         string           `json:"imageId"`
	InstanceType    string           `json:"instanceType"`
	SubnetID        string           `json:"subnetId"`
	SecurityGroupID string           `json:"securityGroupId"`
	RootDevice      string           `json:"rootDevice"`
	DiskGiB         int64            `json:"diskGiB"`
	Network         protocol.Network `json:"network"`
}

// State is what is observed of an instance.
type State struct {
	ImageID      string `json:"imageId"`
	InstanceType string `json:"instanceType"`
	Running      bool   `json:"running"`
}

func ec2(action string, form ...string) protocol.Call {
	return protocol.Call{Method: "POST", Path: "/", Form: append([]string{"Action", "Version"}, form...),
		Fixed: map[string]string{"Action": action, "Version": ec2APIVersion}}
}

var (
	describe = ec2("DescribeInstances", "Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1",
		"Filter.2.Value.2", "Filter.2.Value.3", "Filter.2.Value.4")
	run = func() protocol.Call {
		c := ec2("RunInstances", "MinCount", "MaxCount", "ImageId", "InstanceType", "SubnetId", "SecurityGroupId.1", "UserData",
			"ClientToken", "BlockDeviceMapping.1.DeviceName", "BlockDeviceMapping.1.Ebs.VolumeSize", "BlockDeviceMapping.1.Ebs.VolumeType",
			"MetadataOptions.HttpTokens", "TagSpecification.1.ResourceType",
			"TagSpecification.1.Tag.1.Key", "TagSpecification.1.Tag.1.Value", "TagSpecification.1.Tag.2.Key",
			"TagSpecification.1.Tag.2.Value", "TagSpecification.1.Tag.3.Key", "TagSpecification.1.Tag.3.Value")
		c.Fixed["MinCount"], c.Fixed["MaxCount"] = "1", "1"
		return c
	}()
	start = ec2("StartInstances", "InstanceId.1")
)

// Provider implements sdk.Provider.
type Provider struct {
	// Now is the clock for an instance's uptime (default time.Now).
	Now func() time.Time
}

// Manifest implements sdk.Provider.
func (Provider) Manifest() protocol.Manifest {
	need := func(caps ...string) []protocol.Need {
		var out []protocol.Need
		for _, c := range caps {
			out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "aws-iam", Ref: "{subnetId}"}, Always: true})
		}
		return out
	}
	return protocol.Manifest{
		Schema: protocol.Schema, Provider: "ec2-example", Version: Version, API: API,
		Observe:      need("ec2:DescribeInstances"),
		ObserveCalls: []protocol.Call{describe},
		Kinds: []protocol.Kind{{Kind: Kind, Node: true, Operations: []protocol.Operation{
			{Op: OpCreate, Support: protocol.Supported, Needs: need("ec2:RunInstances", "ec2:CreateTags"), Calls: []protocol.Call{describe, run}},
			{Op: OpStart, Support: protocol.Supported, Needs: need("ec2:StartInstances"), Calls: []protocol.Call{describe, start}},
			{Op: OpChangeType, Support: protocol.Unsupported, Why: "changing an instance's type needs it stopped; do it by hand"},
			{Op: OpReplaceImage, Support: protocol.Unsupported, Why: "changing the image would mean replacing the instance and its data"},
			{Op: OpDeleteVM, Support: protocol.Unsupported, Why: "removing a declaration never terminates an instance"},
		}}},
	}
}

type instance struct {
	InstanceID   string `xml:"instanceId"`
	ImageID      string `xml:"imageId"`
	InstanceType string `xml:"instanceType"`
	ClientToken  string `xml:"clientToken"`
	LaunchTime   string `xml:"launchTime"`
	State        struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	Tags []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
}

func (i instance) tag(k string) (string, bool) {
	for _, t := range i.Tags {
		if t.Key == k {
			return t.Value, true
		}
	}
	return "", false
}

// live: not terminated or being terminated.
var live = []string{"pending", "running", "stopping", "stopped"}

// Observe implements sdk.Provider.
func (p Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	var out protocol.ObserveResult
	seen := map[string]bool{}
	for _, r := range in.Resources {
		if _, err := spec(r.Spec); err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		insts, err := instances(ctx, h, "tag:"+tagName, r.Name)
		if err != nil {
			return out, err
		}
		o := protocol.Observed{ID: r.ID, Kind: Kind, Name: r.Name}
		if len(insts) > 0 {
			p.fill(&o, insts[0])
			seen[insts[0].InstanceID] = true
		}
		out.Resources = append(out.Resources, o)
	}
	// Instances it launched that are no longer declared: reported, so that Mamela keeps them.
	all, err := instances(ctx, h, "tag:"+tagManaged, "true")
	if err != nil {
		return out, err
	}
	for _, i := range all {
		name, ok := i.tag(tagName)
		if seen[i.InstanceID] || !ok {
			continue
		}
		o := protocol.Observed{ID: string(Kind) + " " + name, Kind: Kind, Name: name}
		p.fill(&o, i)
		out.Resources = append(out.Resources, o)
	}
	return out, nil
}

func (p Provider) fill(o *protocol.Observed, i instance) {
	managed, _ := i.tag(tagManaged)
	nodeText, hasNode := i.tag(tagNode)
	node, err := strconv.ParseInt(nodeText, 10, 64)
	o.Exists = true
	// Every mark: the managed tag and Mamela's node number. Either way it is a claim that Mamela
	// checks against the client token it recorded launching it with.
	o.Owned = managed == "true" && hasNode && err == nil && node > 0
	o.Unfinished = managed == "true" && !o.Owned
	o.IDs = map[string]string{"ClientToken": i.ClientToken, "InstanceId.1": i.InstanceID}
	running := i.State.Name == "running" || i.State.Name == "pending"
	if o.Owned {
		o.Node = &protocol.ObservedNode{ID: node, Running: running}
		if launched, err := time.Parse(time.RFC3339, i.LaunchTime); err == nil && running {
			now := time.Now
			if p.Now != nil {
				now = p.Now
			}
			o.Node.UptimeSeconds = max(0, int64(now().Sub(launched).Seconds()))
		}
	}
	o.State, _ = json.Marshal(State{ImageID: i.ImageID, InstanceType: i.InstanceType, Running: running}, json.Deterministic(true))
}

// ClientToken is the token an instance of the resource is launched with: the same for the same
// declaration, so that a retried RunInstances never launches a second instance.
func ClientToken(r protocol.Resource) string {
	sum := sha256.Sum256([]byte(r.ID + "\x00" + string(r.Spec)))
	return "mamela-" + hex.EncodeToString(sum[:12])
}

// Diff implements sdk.Provider.
func (Provider) Diff(ctx context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		obs[o.ID] = o
	}
	var out protocol.DiffResult
	desired := slices.Clone(in.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	for _, r := range desired {
		s, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		o := obs[r.ID]
		switch {
		case !o.Exists:
			token := ClientToken(r)
			out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpCreate,
				IDs:       map[string]string{"ClientToken": token},
				Reasons:   []string{fmt.Sprintf("launch %s from %s in %s, %d GiB, with a mamelet seed", s.InstanceType, s.ImageID, s.SubnetID, s.DiskGiB)},
				Artifacts: []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: r.Name, Network: &s.Network, Format: protocol.SeedUserData}},
				Requests: []protocol.Approved{{Method: run.Method, Path: run.Path, Params: map[string][]string{
					"Action": {"RunInstances"}, "Version": {ec2APIVersion}, "MinCount": {"1"}, "MaxCount": {"1"},
					"ImageId": {s.ImageID}, "InstanceType": {s.InstanceType}, "SubnetId": {s.SubnetID}, "SecurityGroupId.1": {s.SecurityGroupID},
					"UserData": {"{artifact:seed.base64}"}, "ClientToken": {token},
					"BlockDeviceMapping.1.DeviceName": {s.RootDevice}, "BlockDeviceMapping.1.Ebs.VolumeSize": {strconv.FormatInt(s.DiskGiB, 10)},
					"BlockDeviceMapping.1.Ebs.VolumeType": {"gp3"}, "MetadataOptions.HttpTokens": {"required"},
					"TagSpecification.1.ResourceType": {"instance"},
					"TagSpecification.1.Tag.1.Key":    {tagManaged}, "TagSpecification.1.Tag.1.Value": {"true"},
					"TagSpecification.1.Tag.2.Key": {tagName}, "TagSpecification.1.Tag.2.Value": {r.Name},
					"TagSpecification.1.Tag.3.Key": {tagNode}, "TagSpecification.1.Tag.3.Value": {"{artifact:seed.node}"},
				}}}})
		case !o.Owned:
			continue
		default:
			var st State
			if err := json.Unmarshal(o.State, &st); err != nil {
				return out, err
			}
			id := map[string]string{"InstanceId.1": o.IDs["InstanceId.1"]}
			if !st.Running {
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpStart, IDs: id, Reasons: []string{"it is stopped"},
					Requests: []protocol.Approved{{Method: start.Method, Path: start.Path, Params: map[string][]string{
						"Action": {"StartInstances"}, "Version": {ec2APIVersion}, "InstanceId.1": {o.IDs["InstanceId.1"]}}}}})
			}
			if st.InstanceType != s.InstanceType {
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpChangeType, IDs: id,
					Reasons: []string{fmt.Sprintf("instance type %s -> %s", st.InstanceType, s.InstanceType)}})
			}
			if st.ImageID != s.ImageID {
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpReplaceImage, IDs: id,
					Reasons: []string{fmt.Sprintf("image %s -> %s", st.ImageID, s.ImageID)}})
			}
		}
	}
	return out, nil
}

// Apply implements sdk.Provider.
func (Provider) Apply(ctx context.Context, h *sdk.Host, in protocol.ApplyParams) (protocol.ApplyResult, error) {
	c := in.Change
	ready := map[string]protocol.ArtifactInfo{}
	for _, a := range c.Artifacts {
		info, err := h.Artifact(ctx, a.ID)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		ready[a.ID] = info
	}
	for _, a := range c.Requests {
		params, err := protocol.FillArtifacts(a.Params, ready)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		switch params["Action"][0] {
		case "RunInstances":
			// The client token makes a retry harmless; looking first keeps it from being sent at all.
			if got, err := instances(ctx, h, "client-token", params["ClientToken"][0]); err != nil {
				return protocol.ApplyResult{}, err
			} else if len(got) > 0 {
				continue
			}
		case "StartInstances":
			if got, err := instances(ctx, h, "instance-id", params["InstanceId.1"][0]); err != nil {
				return protocol.ApplyResult{}, err
			} else if len(got) > 0 && (got[0].State.Name == "running" || got[0].State.Name == "pending") {
				continue
			}
		}
		if _, err := call(ctx, h, params); err != nil {
			return protocol.ApplyResult{}, err
		}
	}
	return protocol.ApplyResult{Message: string(c.Op) + " " + c.Resource}, nil
}

// Lookup implements sdk.Provider.
func (Provider) Lookup(ctx context.Context, h *sdk.Host, in protocol.LookupParams) (protocol.LookupResult, error) {
	c := in.Change
	var got []instance
	var err error
	if t := c.IDs["ClientToken"]; t != "" {
		got, err = instances(ctx, h, "client-token", t)
	} else {
		got, err = instances(ctx, h, "instance-id", c.IDs["InstanceId.1"])
	}
	if err != nil {
		return protocol.LookupResult{}, err
	}
	switch {
	case len(got) == 0:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	case c.Op == OpStart && got[0].State.Name != "running" && got[0].State.Name != "pending":
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupDone}, nil
}

func spec(v jsontext.Value) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(v, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	switch {
	case !strings.HasPrefix(s.ImageID, "ami-") || !strings.HasPrefix(s.SubnetID, "subnet-") || !strings.HasPrefix(s.SecurityGroupID, "sg-"):
		return s, errors.New("imageId (ami-), subnetId (subnet-) and securityGroupId (sg-) are required")
	case s.InstanceType == "" || s.RootDevice == "" || s.DiskGiB < 8:
		return s, errors.New("instanceType, rootDevice and diskGiB (8 or more) are required")
	case s.Network.Mode != "dhcp":
		return s, errors.New("an instance gets its address from its subnet: network must be dhcp")
	}
	return s, nil
}

// instances describes the live instances where filter has value.
func instances(ctx context.Context, h *sdk.Host, filter, value string) ([]instance, error) {
	params := map[string][]string{"Action": {"DescribeInstances"}, "Version": {ec2APIVersion},
		"Filter.1.Name": {filter}, "Filter.1.Value.1": {value}, "Filter.2.Name": {"instance-state-name"}}
	for i, s := range live {
		params[fmt.Sprintf("Filter.2.Value.%d", i+1)] = []string{s}
	}
	body, err := call(ctx, h, params)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Reservations []struct {
			Instances []instance `xml:"instancesSet>item"`
		} `xml:"reservationSet>item"`
	}
	if err := xml.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("DescribeInstances: %w", err)
	}
	var out []instance
	for _, r := range resp.Reservations {
		out = append(out, r.Instances...)
	}
	slices.SortFunc(out, func(a, b instance) int { return strings.Compare(a.InstanceID, b.InstanceID) })
	return out, nil
}

// call sends a query API request and returns the body of a successful response.
func call(ctx context.Context, h *sdk.Host, params map[string][]string) (string, error) {
	resp, err := h.Request(ctx, protocol.HostRequest{Method: "POST", Path: "/", Form: params})
	if err != nil {
		return "", err
	}
	if resp.Status != 200 {
		return "", fmt.Errorf("%s: status %d %s: %.300s", params["Action"][0], resp.Status, resp.Reason, resp.Body)
	}
	return resp.Body, nil
}
