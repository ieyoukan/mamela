package graph

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/names"
)

// The shapes of node names, package and service names, manifest names, k3s releases and sha256s
// are in internal/names, shared with the revision the Control Plane signs from the declaration:
// what the graph accepts is signed (A11 of the 0.3.2 audit).
var (
	mameletVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	// A provider's kind ("KVItem"), its name ("kv-example"), and a resource name.
	providedKindRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,63}$`)
	providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	providedNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,252}$`)
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
	nodes := map[string]bool{}       // the names of the provider's resources: a node is one of them (checkNodeRef)
	addresses := map[string]string{} // static address -> machine
	for _, r := range g.Resources {
		if seen[r.ID()] {
			add(r, CodeDuplicateResource, "", fmt.Sprintf("%s is declared more than once, differently", r.ID()),
				"declare it once, or the same way each time (a resource declared twice the same way is one), or give them different names")
		}
		seen[r.ID()] = true
		// A node is a resource of a provider whose kind is a node kind (the provider says which;
		// checked once providers run, ADR 0004).
		if r.Provided != nil {
			nodes[r.Name] = true
		}
	}
	v := &Validator{g: g, nodes: nodes, add: add}

	for _, r := range g.Resources {
		if p := r.Provided; p != nil {
			if !providedKindRe.MatchString(string(r.Kind)) {
				add(r, CodeInvalidName, "kind", fmt.Sprintf("kind %q is not a valid kind name", r.Kind),
					"a kind is an uppercase letter and up to 63 letters and digits, such as \"KVItem\"")
			}
			if !providerNameRe.MatchString(p.Provider) {
				add(r, CodeInvalidName, "provider", fmt.Sprintf("provider %q is not a valid provider name", p.Provider),
					"a provider name is lowercase letters, digits and '-'")
			}
			if !providedNameRe.MatchString(r.Name) {
				add(r, CodeInvalidName, "name", fmt.Sprintf("name %q is not a valid resource name", r.Name),
					"use letters, digits and '.', '_', ':', '@', '-', starting with a letter or digit")
			}
			// A machine's network (spec.network, the Mamela Pome's Network): well formed, and each
			// static address declared once.
			n, hasNetwork, err := r.CheckedNetwork()
			if err != nil {
				// Not skipped: a network of the wrong shape must not pass as no network at all.
				add(r, CodeInvalidValue, "spec.network", "spec.network is not a network: "+networkShape(err), networkHint)
			}
			if hasNetwork && err == nil {
				for _, p := range checkNetwork(n) {
					add(r, CodeInvalidValue, "spec.network", p, networkHint)
				}
				if ip := n.IP(); ip != "" && n.Mode == NetworkStatic {
					if other, dup := addresses[ip]; dup {
						add(r, CodeDuplicateAddress, "spec.network.address", fmt.Sprintf("address %s is also declared for machine %s", ip, other),
							"give each machine its own address")
					} else {
						addresses[ip] = r.Name
					}
				}
			}
			continue
		}
		switch r.Kind {
		case KindNodePackage:
			s := r.NodePackage
			checkNodeRef(add, r, s.Node, nodes)
			if !names.Package(s.Package) {
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
		case KindNodeMamelet:
			s := r.NodeMamelet
			checkNodeRef(add, r, s.Node, nodes)
			if !names.SHA256(s.SHA256) {
				add(r, CodeInvalidValue, "spec.sha256", fmt.Sprintf("%q is not a sha256 in lowercase hex", s.SHA256),
					"the sha256 of the linux/amd64 mamelet binary: shasum -a 256 mamelet-linux-amd64")
			}
			if s.Version != "" && !mameletVersionRe.MatchString(s.Version) {
				add(r, CodeInvalidValue, "spec.version", fmt.Sprintf("%q is not a valid version label", s.Version),
					"letters, digits and '.', '_', '+', '-', at most 64")
			}
			if want := s.Node + "/mamelet"; r.Name != want {
				add(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/mamelet)", want), "")
			}
		case KindNodeService:
			s := r.NodeService
			checkNodeRef(add, r, s.Node, nodes)
			if !names.Service(s.Service) {
				add(r, CodeInvalidValue, "spec.service", fmt.Sprintf("%q is not a valid systemd service name", s.Service),
					"write the unit name without \".service\", for example \"nginx\"")
			}
			if want := s.Node + "/" + s.Service; r.Name != want {
				add(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/service)", want), "")
			}
		default:
			if d, ok := extKinds[r.Kind]; ok {
				d.Validate(v, r)
			}
		}
	}
	// after: each names a resource of a provider in this graph, and none waits for itself.
	provided := map[string]Resource{}
	for _, r := range g.Resources {
		if r.Provided != nil {
			provided[r.ID()] = r
		}
	}
	for _, r := range g.Resources {
		for _, a := range r.After {
			if _, ok := provided[a]; !ok {
				add(r, CodeDanglingNode, "after", fmt.Sprintf("%q is not a resource of a provider in this graph", a),
					"declare it, or fix the reference (\"Kind name\")")
			}
		}
	}
	ways := cycles(provided)
	for _, id := range slices.Sorted(maps.Keys(ways)) {
		r := provided[id]
		add(r, CodeCycle, "after", fmt.Sprintf("%s needs itself through after: %s", id, strings.Join(ways[id], " -> ")),
			"remove one of the dependencies on that way")
	}
	checkUnits(g, add)
	for _, e := range extensions {
		if e.Check != nil {
			e.Check(g, v)
		}
	}
	return newError(problems)
}

// cycles finds the resources that wait for themselves through after, and for each the way round,
// starting and ending at it: ["Thing a", "Thing b", "Thing a"]. A resource on several cycles has
// the first one found (the search goes in the order of the names, so it is always the same).
func cycles(rs map[string]Resource) map[string][]string {
	state := map[string]int{} // 0 new, 1 on the path, 2 done
	ways := map[string][]string{}
	var path []string
	var visit func(id string)
	visit = func(id string) {
		state[id] = 1
		path = append(path, id)
		for _, a := range rs[id].After {
			switch state[a] {
			case 0:
				if _, ok := rs[a]; ok {
					visit(a)
				}
			case 1:
				i := slices.Index(path, a)
				ring := path[i:] // a ... id, and id needs a
				for k, member := range ring {
					if _, done := ways[member]; done {
						continue
					}
					way := append(slices.Clone(ring[k:]), ring[:k]...)
					ways[member] = append(way, member)
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = 2
	}
	for _, id := range slices.Sorted(maps.Keys(rs)) {
		if state[id] == 0 {
			visit(id)
		}
	}
	return ways
}

// checkUnits refuses two declarations of one thing that contradict each other: the same systemd
// unit of a node declared twice, once as nginx and once as nginx.service, one running and one not
// (the revision would carry both, and the unit would be started and stopped in turn). What
// contradicts a node extension's declaration (k3s's service) is the extension's Check.
func checkUnits(g *Graph, add addFunc) {
	first := map[string]Resource{} // node and unit -> the first declaration of it
	for _, r := range g.Resources {
		s := r.NodeService
		if s == nil {
			continue
		}
		unit := strings.TrimSuffix(s.Service, ".service")
		if other, ok := first[s.Node+"\x00"+unit]; !ok {
			first[s.Node+"\x00"+unit] = r
		} else if o := other.NodeService; o.Enabled != s.Enabled || o.Running != s.Running {
			add(r, CodeConflict, "spec", fmt.Sprintf("%s declares the unit %s as enabled=%t running=%t, but %s declares the same unit as enabled=%t running=%t",
				r.ID(), unit, s.Enabled, s.Running, other.ID(), o.Enabled, o.Running),
				"declare a unit once (nginx and nginx.service are the same unit), or declare it the same way each time")
		}
	}
}

type addFunc func(r Resource, code Code, field, message, hint string)

// ValidNodeName reports whether a machine's name can be its hostname (a DNS label): checked for a
// provider's node kinds once the providers say which kinds are machines.
func ValidNodeName(name string) bool { return names.Node(name) }

// checkNodeRef checks that a node resource names a resource of a provider. Which of the resources
// of that name is a machine (a node kind) is for the providers' manifests to say, not the graph:
// a machine "web" may have a disk "web" of another kind. Once the providers run, CheckGraph
// (internal/provider/external) refuses a name that is two machines (MAM-GRAPH-0103) and a node
// that is no machine.
func checkNodeRef(add addFunc, r Resource, node string, nodes map[string]bool) {
	if !nodes[node] {
		add(r, CodeDanglingNode, "spec.node", fmt.Sprintf("node %q is not declared in this graph (as a provider's machine)", node),
			"declare the machine in the same graph, or fix the node name")
	}
}

// networkShape says in a few words how a spec.network differs from what the Mamela Pome's Network
// writes (the JSON library's message names Go types, which mean nothing to a person).
func networkShape(err error) string {
	var sem *json.SemanticError
	switch {
	case errors.Is(err, json.ErrUnknownName):
		return "it has a member other than mode, address, gateway and dns (" + lastLine(err.Error()) + ")"
	case errors.As(err, &sem):
		return "mode, address and gateway are strings and dns is a list of strings"
	}
	return "it is one object of mode, address, gateway and dns"
}

// lastLine is the part of a JSON library message that names the member.
func lastLine(s string) string {
	if _, after, ok := strings.Cut(s, ": "); ok {
		return after
	}
	return s
}

const networkHint = "declare Dhcp, or Static(address: \"192.0.2.10/24\", gateway: \"192.0.2.1\", dns: [\"192.0.2.1\"])"

// checkNetwork says what is wrong with a machine's network, if anything.
func checkNetwork(n Network) []string {
	switch n.Mode {
	case NetworkDHCP:
		if n.Address != "" || n.Gateway != "" || len(n.DNS) > 0 {
			return []string{"a DHCP network has no address, gateway or name servers of its own"}
		}
		return nil
	case NetworkStatic:
	default:
		return []string{fmt.Sprintf("network mode %q is not dhcp or static", n.Mode)}
	}
	var out []string
	prefix, err := netip.ParsePrefix(n.Address)
	switch {
	case err != nil || !prefix.Addr().Is4():
		return []string{fmt.Sprintf("address %q is not an IPv4 address with a prefix length, such as 192.0.2.10/24", n.Address)}
	case prefix.Bits() < 8 || prefix.Bits() > 30:
		out = append(out, fmt.Sprintf("prefix length /%d is outside /8../30", prefix.Bits()))
	}
	addr, subnet := prefix.Addr(), prefix.Masked()
	if !addr.IsGlobalUnicast() || addr.IsLoopback() {
		out = append(out, fmt.Sprintf("address %s is not a unicast host address", addr))
	} else if addr == subnet.Addr() || addr == lastAddr(subnet) {
		out = append(out, fmt.Sprintf("address %s is the network or broadcast address of %s", addr, subnet))
	}
	gw, err := netip.ParseAddr(n.Gateway)
	switch {
	case err != nil || !gw.Is4():
		out = append(out, fmt.Sprintf("gateway %q is not an IPv4 address", n.Gateway))
	case !subnet.Contains(gw):
		out = append(out, fmt.Sprintf("gateway %s is not in the address's subnet %s", gw, subnet))
	case gw == addr:
		out = append(out, fmt.Sprintf("gateway %s is the machine's own address", gw))
	}
	if len(n.DNS) < 1 || len(n.DNS) > 3 {
		out = append(out, fmt.Sprintf("%d name servers; declare 1 to 3", len(n.DNS)))
	}
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			out = append(out, fmt.Sprintf("name server %q is not an IPv4 address", d))
		}
	}
	return out
}

// lastAddr is the last (broadcast) address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
