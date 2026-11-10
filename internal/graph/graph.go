// Package graph decodes, validates and canonicalizes the mamela-graph/v1 Resource Graph.
//
// The graph comes from an untrusted evaluation (Emela compiled to WASM), so decoding is strict:
// unknown schemas, kinds and fields, duplicate members, non-integer numbers and trailing data are
// all rejected. Everything here is a pure function of its input, so the same text always gives
// the same graph, the same problems and the same digest.
package graph

// Schema is the only accepted value of the "schema" member.
const Schema = "mamela-graph/v1"

// Kind names a resource type.
type Kind string

const (
	KindVirtualMachine Kind = "VirtualMachine"
	KindNodePackage    Kind = "NodePackage"
	KindNodeService    Kind = "NodeService"
)

// kindOrder fixes the canonical order of kinds: nodes before what lives on them.
var kindOrder = map[Kind]int{
	KindVirtualMachine: 0,
	KindNodePackage:    1,
	KindNodeService:    2,
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

// NodeServiceSpec is the desired state of a systemd service on a node.
type NodeServiceSpec struct {
	Node    string
	Service string
	Enabled bool
	Running bool
}
