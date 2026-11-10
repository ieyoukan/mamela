package graph

import (
	"fmt"
	"regexp"
)

// Limits on declared values. They are deliberately conservative; raising one is a reviewed change.
const (
	MinCPU       = 1
	MaxCPU       = 64
	MinMemoryMiB = 256
	MaxMemoryMiB = 1 << 20 // 1 TiB
	MinDiskGiB   = 1
	MaxDiskGiB   = 16384
)

var (
	// A node name is a DNS label: it becomes a hostname.
	nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Debian package names (Debian Policy 5.6.1).
	packageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	// systemd unit name without the ".service" suffix; no path separators or whitespace.
	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,254}$`)
	// An image reference such as "ubuntu-24.04".
	imageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

// Validate checks the meaning of a decoded graph: names, ranges, references and duplicates.
// It reports every problem at once, in a deterministic order.
func Validate(g *Graph) error {
	var problems []Problem
	add := func(r Resource, code Code, field, message, hint string) {
		path := r.ID()
		if field != "" {
			path += " " + field
		}
		problems = append(problems, Problem{Code: code, Path: path, Message: message, Hint: hint})
	}

	seen := map[string]bool{}
	nodes := map[string]bool{}
	for _, r := range g.Resources {
		if seen[r.ID()] {
			add(r, CodeDuplicateResource, "", fmt.Sprintf("%s is declared more than once", r.ID()),
				"remove one of the declarations or give them different names")
		}
		seen[r.ID()] = true
		if r.Kind == KindVirtualMachine {
			nodes[r.Name] = true
		}
	}

	for _, r := range g.Resources {
		switch r.Kind {
		case KindVirtualMachine:
			s := r.VirtualMachine
			if !nodeNameRe.MatchString(r.Name) {
				add(r, CodeInvalidName, "name", fmt.Sprintf("VM name %q is not a valid hostname", r.Name),
					"use 1-63 lowercase letters, digits and '-', starting and ending with a letter or digit")
			}
			checkRange(add, r, "spec.cpu", s.CPU, MinCPU, MaxCPU, "")
			checkRange(add, r, "spec.memoryMiB", s.MemoryMiB, MinMemoryMiB, MaxMemoryMiB, " MiB")
			checkRange(add, r, "spec.diskGiB", s.DiskGiB, MinDiskGiB, MaxDiskGiB, " GiB")
			if !imageRe.MatchString(s.Image) {
				add(r, CodeInvalidValue, "spec.image", fmt.Sprintf("image %q is not a valid image name", s.Image),
					"use a name such as \"ubuntu-24.04\"")
			}
		case KindNodePackage:
			s := r.NodePackage
			checkNodeRef(add, r, s.Node, nodes)
			if !packageNameRe.MatchString(s.Package) {
				add(r, CodeInvalidValue, "spec.package", fmt.Sprintf("%q is not a valid Debian package name", s.Package),
					"package names use lowercase letters, digits and '+', '-', '.'")
			}
			if s.State != PackagePresent {
				add(r, CodeInvalidValue, "spec.state", fmt.Sprintf("package state %q is not supported", s.State),
					"only \"Present\" can be declared; Mamela never removes packages because a declaration says so")
			}
			if want := s.Node + "/" + s.Package; r.Name != want {
				add(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/package)", want), "")
			}
		case KindNodeService:
			s := r.NodeService
			checkNodeRef(add, r, s.Node, nodes)
			if !serviceNameRe.MatchString(s.Service) {
				add(r, CodeInvalidValue, "spec.service", fmt.Sprintf("%q is not a valid systemd service name", s.Service),
					"write the unit name without \".service\", for example \"nginx\"")
			}
			if want := s.Node + "/" + s.Service; r.Name != want {
				add(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/service)", want), "")
			}
		}
	}
	return newError(problems)
}

type addFunc func(r Resource, code Code, field, message, hint string)

func checkRange(add addFunc, r Resource, field string, v, lo, hi int64, unit string) {
	if v < lo || v > hi {
		add(r, CodeOutOfRange, field, fmt.Sprintf("%s is %d%s, outside %d%s to %d%s", field, v, unit, lo, unit, hi, unit), "")
	}
}

func checkNodeRef(add addFunc, r Resource, node string, nodes map[string]bool) {
	if !nodes[node] {
		add(r, CodeDanglingNode, "spec.node", fmt.Sprintf("node %q is not declared as a VirtualMachine in this graph", node),
			"declare the VM in the same graph, or fix the node name")
	}
}
