// Package graph decodes, validates and canonicalizes the mamela-graph/v1 Resource Graph.
//
// The graph comes from an untrusted evaluation (Emela compiled to WASM), so decoding is strict:
// unknown schemas, kinds and fields, duplicate members, non-integer numbers and trailing data are
// all rejected. Everything here is a pure function of its input, so the same text always gives
// the same graph, the same problems and the same digest.
package graph

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Schema is the schema of a graph of built-in kinds only.
const Schema = "mamela-graph/v1"

// SchemaV2 is the schema of a graph that also has resources of providers (Resource.Provided).
// A v1 graph is a v2 graph without them; Mamela reads both and writes v1 when it can.
const SchemaV2 = "mamela-graph/v2"

// Kind names a resource type. It is the provider protocol's ResourceKind, so that manifests name
// kinds with the same type.
type Kind = protocol.ResourceKind

const (
	KindNodePackage Kind = "NodePackage"
	KindNodeService Kind = "NodeService"
	// KindNodeMamelet is the mamelet release a node runs: its sha256 is declared, like a k3s
	// binary's, and a node that runs another one is updated (with an approval).
	KindNodeMamelet Kind = "NodeMamelet"
)

// CoreKind reports whether a kind is one of the core's own (packages, services, mamelet), not a node
// extension's.
func CoreKind(k Kind) bool {
	return k == KindNodePackage || k == KindNodeService || k == KindNodeMamelet
}

// Builtin reports whether a kind is one of Mamela's own, the core's or a node extension's (not a
// provider's). Machines are providers' node kinds (ADR 0004, 0006); the built-in kinds are what
// lives on them.
func Builtin(k Kind) bool {
	_, ext := extKinds[k]
	return CoreKind(k) || ext
}

// Graph is a decoded and validated desired state.
type Graph struct {
	Resources []Resource
}

// Resource is one desired resource. Exactly one of the spec pointers (or Ext) is set, matching Kind.
type Resource struct {
	Kind Kind
	Name string

	NodePackage *NodePackageSpec
	NodeService *NodeServiceSpec
	NodeMamelet *NodeMameletSpec
	// Ext is the spec of a node extension's kind (KindOf): the extension's own type, which the
	// core does not look into.
	Ext any
	// Provided is set for a resource of a provider's kind: Mamela does not know the kind; the
	// provider does (its manifest and its diff).
	Provided *ProvidedSpec
	// After are the resources of providers this one needs first ("Kind name", sorted, ADR 0007):
	// it is planned once each exists and is Mamela's.
	After []string
}

// ProvidedSpec is a resource of a provider's kind.
type ProvidedSpec struct {
	Provider string // the provider's name, as in its manifest
	// Spec is the declared fields, a JSON object in canonical form (RFC 8785): Mamela passes it to
	// the provider as is.
	Spec []byte
}

// ID is the identity of a resource within a graph.
func (r Resource) ID() string { return string(r.Kind) + " " + r.Name }

// Node is the machine a resource of a built-in kind lives on ("" for a provider's resource, which
// is not on a machine).
func (r Resource) Node() string {
	switch {
	case r.NodePackage != nil:
		return r.NodePackage.Node
	case r.NodeService != nil:
		return r.NodeService.Node
	case r.NodeMamelet != nil:
		return r.NodeMamelet.Node
	case r.Ext != nil:
		if d, ok := extKinds[r.Kind]; ok {
			return d.Node(r.Ext)
		}
	}
	return ""
}

// NetworkMode is how a machine gets its IPv4 address. A declaration always says which: there is no
// default.
type NetworkMode string

const (
	NetworkDHCP   NetworkMode = "dhcp"
	NetworkStatic NetworkMode = "static"
)

// Network is a machine's IPv4 configuration, given to it at creation (its seed). A provider's
// machine declares it as spec.network: {"mode": "dhcp"} or {"mode": "static", "address": ...,
// "gateway": ..., "dns": [...]} (the Mamela Pome's Network).
type Network struct {
	Mode    NetworkMode
	Address string   // static: the address with its prefix length, such as "192.0.2.10/24"
	Gateway string   // static: the default gateway, in the address's subnet
	DNS     []string // static: name servers, 1 to 3
}

// IP is a static network's address without the prefix length ("" for DHCP).
func (n Network) IP() string {
	ip, _, _ := strings.Cut(n.Address, "/")
	return ip
}

// Allowed says whether a site that allows DHCP (dhcp) and static addresses from first to last
// (both included; invalid when the site has no range) accepts this network, and why not.
func (n Network) Allowed(dhcp bool, first, last netip.Addr) error {
	switch n.Mode {
	case NetworkDHCP:
		if !dhcp {
			return fmt.Errorf("the site does not allow DHCP (network.dhcp = false); declare a static address from its range")
		}
		return nil
	case NetworkStatic:
		ip, err := netip.ParseAddr(n.IP())
		if err != nil {
			return fmt.Errorf("address %q is not an IPv4 address", n.Address)
		}
		if !first.IsValid() || !last.IsValid() {
			return fmt.Errorf("static address %s: the site has no range for static addresses (network.static_first and static_last)", ip)
		}
		if ip.Less(first) || last.Less(ip) {
			return fmt.Errorf("static address %s is outside the site's range %s-%s", ip, first, last)
		}
		return nil
	}
	return fmt.Errorf("network mode %q is not dhcp or static", n.Mode)
}

// String is the network as one line, as plans show it and the ownership marker records it.
func (n Network) String() string {
	if n.Mode != NetworkStatic {
		return string(n.Mode)
	}
	return fmt.Sprintf("static %s via %s dns %s", n.Address, n.Gateway, strings.Join(n.DNS, ","))
}

// PackageState is the desired state of a package. Only Present exists: removal is never declared.
type PackageState string

const PackagePresent PackageState = "Present"

// NodePackageSpec says an APT package should be present on a node.
type NodePackageSpec struct {
	Node    string
	Package string
	State   PackageState
}

// NodeMameletSpec says a node runs the mamelet whose binary has this sha256. The Control Plane
// serves that binary (the site's controlplane.mamelet_binary); mamelet on the node fetches it,
// checks the sha256, keeps the previous binary and restarts into the new one. The resource's name
// is "<node>/mamelet".
type NodeMameletSpec struct {
	Node    string
	Version string // a label for people (shown in the plan); optional
	SHA256  string // hex, of the linux/amd64 binary
}

// NodeServiceSpec is the desired state of a systemd service on a node.
type NodeServiceSpec struct {
	Node    string
	Service string
	Enabled bool
	Running bool
}

// Network is the network a provider's machine declares (spec.network), if it declares one and it
// is well formed (CheckedNetwork says what is wrong with one that is not).
func (r Resource) Network() (Network, bool) {
	n, ok, err := r.CheckedNetwork()
	return n, ok && err == nil
}

// wireNetwork is spec.network as the Mamela Pome's Network writes it: exactly these members, of
// these types.
type wireNetwork struct {
	Mode    string   `json:"mode"`
	Address string   `json:"address"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

// CheckedNetwork is the network a provider's resource declares: ok is whether it has a
// spec.network at all, and err says why that is not a network (a string, a list, a member of
// another type, a member that is none of mode, address, gateway and dns). The members' values are
// checked by Validate.
func (r Resource) CheckedNetwork() (n Network, ok bool, err error) {
	if r.Provided == nil {
		return Network{}, false, nil
	}
	var spec struct {
		Network jsontext.Value `json:"network"`
	}
	if err := json.Unmarshal(r.Provided.Spec, &spec); err != nil {
		return Network{}, false, err
	}
	if spec.Network == nil {
		return Network{}, false, nil
	}
	var w wireNetwork
	if err := json.Unmarshal(spec.Network, &w, strict); err != nil {
		return Network{}, true, err
	}
	return Network{Mode: NetworkMode(w.Mode), Address: w.Address, Gateway: w.Gateway, DNS: w.DNS}, true, nil
}

// StaticIP is the declared static address of machine name, or "" when it uses DHCP or is not
// declared.
func (g *Graph) StaticIP(name string) string {
	for _, r := range g.Resources {
		if n, ok := r.Network(); ok && r.Name == name && n.Mode == NetworkStatic {
			return n.IP()
		}
	}
	return ""
}
