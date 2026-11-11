package graph

import (
	"fmt"
	"net/netip"
	"regexp"
)

var (
	// A node name is a DNS label: it becomes a hostname.
	nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Debian package names (Debian Policy 5.6.1).
	packageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	// systemd unit name without the ".service" suffix; no path separators or whitespace.
	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,254}$`)
	// A provider's kind ("KVItem"), its name ("kv-example"), and a resource name.
	// A k3s release ("v1.33.5+k3s1") and a sha256 in hex.
	K3sVersionRe   = regexp.MustCompile(`^v1\.[0-9]{1,2}\.[0-9]{1,2}\+k3s[0-9]{1,2}$`)
	sha256Re       = regexp.MustCompile(`^[0-9a-f]{64}$`)
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
	nodes := map[string]int{} // name -> how many resources could be the node of that name
	k3sServers := map[string]bool{}
	addresses := map[string]string{} // static address -> machine
	for _, r := range g.Resources {
		if seen[r.ID()] {
			add(r, CodeDuplicateResource, "", fmt.Sprintf("%s is declared more than once", r.ID()),
				"remove one of the declarations or give them different names")
		}
		seen[r.ID()] = true
		// A node is a resource of a provider whose kind is a node kind (the provider says which;
		// checked once providers run, ADR 0004).
		if r.Provided != nil {
			nodes[r.Name]++
		}
		if r.NodeK3s != nil && r.NodeK3s.Role == K3sServer {
			k3sServers[r.NodeK3s.Node] = true
		}
	}

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
			if n, ok := r.Network(); ok {
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
		case KindNodeK3s:
			s := r.NodeK3s
			checkNodeRef(add, r, s.Node, nodes)
			if !K3sVersionRe.MatchString(s.Version) {
				add(r, CodeInvalidValue, "spec.version", fmt.Sprintf("%q is not a k3s release", s.Version),
					"write the release tag, such as \"v1.33.5+k3s1\"")
			}
			if !sha256Re.MatchString(s.SHA256) {
				add(r, CodeInvalidValue, "spec.sha256", "sha256 is not 64 lowercase hex digits",
					"copy it from the release's sha256sum file for the node's architecture")
			}
			if want := s.Node + "/k3s"; r.Name != want {
				add(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/k3s)", want), "")
			}
			switch {
			case s.Role == K3sServer && s.Server != "":
				add(r, CodeInvalidValue, "spec.server", "a k3s server has no server", "")
			case s.Role == K3sAgent && !k3sServers[s.Server]:
				add(r, CodeDanglingNode, "spec.server", fmt.Sprintf("%q is not a node declared as a k3s server in this graph", s.Server),
					"declare the server with Mamela.k3s_server on that node")
			case s.Role == K3sAgent && s.Server == s.Node:
				add(r, CodeInvalidValue, "spec.server", "a node cannot be an agent of itself", "")
			case s.Role != K3sServer && s.Role != K3sAgent:
				add(r, CodeInvalidValue, "spec.role", fmt.Sprintf("role %q is not server or agent", s.Role), "")
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

// ValidNodeName reports whether a machine's name can be its hostname (a DNS label): checked for a
// provider's node kinds once the providers say which kinds are machines.
func ValidNodeName(name string) bool { return nodeNameRe.MatchString(name) }

func checkNodeRef(add addFunc, r Resource, node string, nodes map[string]int) {
	switch nodes[node] {
	case 0:
		add(r, CodeDanglingNode, "spec.node", fmt.Sprintf("node %q is not declared in this graph (as a provider's machine)", node),
			"declare the machine in the same graph, or fix the node name")
	case 1:
	default:
		add(r, CodeDanglingNode, "spec.node", fmt.Sprintf("node %q names more than one resource; a node's name must be unique", node),
			"rename one of them")
	}
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
