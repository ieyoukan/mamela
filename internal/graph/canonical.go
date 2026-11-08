package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"sort"
)

// Canonical types: members are declared in byte order of their JSON names, so the encoding has
// sorted keys, the same form the Emela writer produces. Do not reorder fields.
type canonGraph struct {
	Resources []canonResource `json:"resources"`
	Schema    string          `json:"schema"`
}

type canonResource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Spec any    `json:"spec"`
}

type canonVirtualMachine struct {
	CPU       int64  `json:"cpu"`
	DiskGiB   int64  `json:"diskGiB"`
	Image     string `json:"image"`
	MemoryMiB int64  `json:"memoryMiB"`
}

type canonNodePackage struct {
	Node    string `json:"node"`
	Package string `json:"package"`
	State   string `json:"state"`
}

type canonNodeService struct {
	Enabled bool   `json:"enabled"`
	Node    string `json:"node"`
	Running bool   `json:"running"`
	Service string `json:"service"`
}

// Sorted returns the resources in canonical order: by kind (nodes first), then by name.
// Declaration order carries no meaning, so reordering a declaration does not change the digest.
func (g *Graph) Sorted() []Resource {
	rs := append([]Resource(nil), g.Resources...)
	sort.SliceStable(rs, func(i, j int) bool {
		if kindOrder[rs[i].Kind] != kindOrder[rs[j].Kind] {
			return kindOrder[rs[i].Kind] < kindOrder[rs[j].Kind]
		}
		return rs[i].Name < rs[j].Name
	})
	return rs
}

// Canonical returns the canonical mamela-graph/v1 encoding: resources in canonical order,
// object keys sorted, no whitespace, no trailing newline.
func (g *Graph) Canonical() []byte {
	cg := canonGraph{Schema: Schema, Resources: []canonResource{}}
	for _, r := range g.Sorted() {
		cr := canonResource{Kind: string(r.Kind), Name: r.Name}
		switch r.Kind {
		case KindVirtualMachine:
			s := r.VirtualMachine
			cr.Spec = canonVirtualMachine{CPU: s.CPU, DiskGiB: s.DiskGiB, Image: s.Image, MemoryMiB: s.MemoryMiB}
		case KindNodePackage:
			s := r.NodePackage
			cr.Spec = canonNodePackage{Node: s.Node, Package: s.Package, State: string(s.State)}
		case KindNodeService:
			s := r.NodeService
			cr.Spec = canonNodeService{Enabled: s.Enabled, Node: s.Node, Running: s.Running, Service: s.Service}
		}
		cg.Resources = append(cg.Resources, cr)
	}
	out, err := json.Marshal(cg, json.Deterministic(true))
	if err != nil {
		// Only plain strings, integers and booleans are encoded; failure is a programming error.
		panic("graph: canonical encoding failed: " + err.Error())
	}
	return out
}

// Digest is "sha256:" followed by the hex SHA-256 of the canonical encoding.
func (g *Graph) Digest() string {
	sum := sha256.Sum256(g.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}
