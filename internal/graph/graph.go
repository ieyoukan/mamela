// Package graph decodes, validates and canonicalizes the mamela-graph/v1 Resource Graph.
//
// The graph comes from an untrusted evaluation (Emela compiled to WASM), so decoding is strict:
// unknown schemas, kinds and fields, duplicate members, non-integer numbers and trailing data are
// all rejected. Everything here is a pure function of its input, so the same text always gives
// the same graph, the same problems and the same digest.
package graph

import "github.com/ieyoukan/mamela/provider/protocol"

// Schema is the schema of a graph of built-in kinds only.
const Schema = "mamela-graph/v1"

// SchemaV2 is the schema of a graph that also has resources of providers (Resource.Provided).
// A v1 graph is a v2 graph without them; Mamela reads both and writes v1 when it can.
const SchemaV2 = "mamela-graph/v2"

// Kind names a resource type. It is the provider protocol's ResourceKind, so that manifests name
// kinds with the same type.
type Kind = protocol.ResourceKind

const (
	KindVirtualMachine Kind = "VirtualMachine"
	KindNodePackage    Kind = "NodePackage"
	KindNodeService    Kind = "NodeService"
	KindNodeK3s        Kind = "NodeK3s"
)

// Builtin reports whether a kind is one of Mamela's own (not a provider's).
func Builtin(k Kind) bool {
	_, ok := kindOrder[k]
	return ok
}

// kindOrder fixes the canonical order of kinds: nodes before what lives on them.
var kindOrder = map[Kind]int{
	KindVirtualMachine: 0,
	KindNodePackage:    1,
	KindNodeService:    2,
	KindNodeK3s:        3,
}

// Graph is a decoded and validated desired state.
type Graph struct {
	Resources []Resource
}

// Resource is one desired resource. Exactly one of the spec pointers is set, matching Kind.
type Resource struct {
	Kind Kind
	Name string

	VirtualMachine *VirtualMachineSpec
	NodePackage    *NodePackageSpec
	NodeService    *NodeServiceSpec
	NodeK3s        *NodeK3sSpec
	// Provided is set for a resource of a provider's kind: Mamela does not know the kind; the
	// provider does (its manifest and its diff).
	Provided *ProvidedSpec
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

// VirtualMachineSpec is the desired shape of a VM.
type VirtualMachineSpec struct {
	CPU       int64
	MemoryMiB int64
	DiskGiB   int64
	Image     string
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

// NodeK3sSpec says a node runs k3s: this exact release, whose binary for the node's architecture
// has this sha256, as a server or as an agent of the server on another node. Mamela writes the
// systemd unit itself; the Control Plane gives the cluster's nodes its join token. Nothing else of
// the node's configuration comes from the declaration.
type NodeK3sSpec struct {
	Node    string
	Role    K3sRole
	Server  string // for an agent: the VM of its server
	Version string // "v1.33.5+k3s1"
	SHA256  string // hex, of the release's binary for the node's architecture
}

// K3sRole is what a node is in its k3s cluster.
type K3sRole string

const (
	K3sServer K3sRole = "server"
	K3sAgent  K3sRole = "agent"
)

// NodeServiceSpec is the desired state of a systemd service on a node.
type NodeServiceSpec struct {
	Node    string
	Service string
	Enabled bool
	Running bool
}
