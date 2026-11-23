package linux

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
)

// The extension's graph warnings (the ranges of each kind are in codes.go).
const (
	// CodeSecret: a file's content looks like a secret (a private key, a token).
	CodeSecret graph.Code = "MAM-GRAPH-0203"
	// CodeRestartUndeclared: a file restarts a service that the declaration does not have on the node.
	CodeRestartUndeclared graph.Code = "MAM-GRAPH-0204"
)

// claim is a path a resource writes, and the resource.
type claim struct {
	path string
	id   string
}

// checkGraph checks what concerns more than one resource of the extension: on one node, no path is
// claimed twice, nor a path and a directory above it (a file cannot be both), nor a path in a
// declared user's home; the approved writes of a node's files fit in its revision (MaxNodeContent);
// and a node has no more of a kind than mamelet observes.
func checkGraph(g *graph.Graph, v *graph.Validator) {
	byNode := map[string][]claim{}
	total := map[string]int{}
	for _, r := range g.Sorted() {
		k, ok := kindOf(r.Kind)
		if !ok || k.claims == nil {
			continue
		}
		node, path, ok := k.claims(r)
		if !ok || CheckPath(path) != nil {
			continue // its own problem, already
		}
		for _, c := range byNode[node] {
			switch {
			case c.path == path:
				v.Problem(r, graph.CodeConflict, "spec.path", fmt.Sprintf("%s is also declared by %s on %s", path, c.id, node),
					"declare the file once")
			case Under(path, c.path):
				v.Problem(r, graph.CodeConflict, "spec.path", fmt.Sprintf("%s is under %s, which %s declares as a file on %s", path, c.path, c.id, node),
					"a path is a file or a directory, not both: change one of the paths")
			case Under(c.path, path):
				v.Problem(r, graph.CodeConflict, "spec.path", fmt.Sprintf("%s is a directory of %s, which %s declares on %s", path, c.path, c.id, node),
					"a path is a file or a directory, not both: change one of the paths")
			}
		}
		byNode[node] = append(byNode[node], claim{path, r.ID()})
		if k.content == nil {
			continue
		}
		before := total[node]
		total[node] += k.content(r)
		if before <= MaxNodeContent && total[node] > MaxNodeContent {
			v.Problem(r, graph.CodeInvalidValue, "spec.content", fmt.Sprintf("the files declared on %s hold more than %d bytes of content (%d with this one): their approved writes ride in the node's revision",
				node, MaxNodeContent, total[node]),
				"put large content into a download pinned by its sha256 (NodeArtifact), or spread the files over fewer at a time")
		}
	}
	checkAptRepos(g, v) // NodeAptRepo
	checkHomes(g, v, byNode)
	checkCounts(g, v)
}

// checkHomes refuses, on one node, a file (NodeFile, NodeArtifact) in the home of a user the
// declaration has: the home is the user's, who can replace what root puts in it (and a directory
// mamelet made above the file would be root's, in the user's way); and a home that is a file's path
// or under it.
func checkHomes(g *graph.Graph, v *graph.Validator, claims map[string][]claim) {
	for _, r := range g.Sorted() {
		u := UserOf(r)
		if u == nil {
			continue
		}
		home, _ := u.CreateHome()
		if home == "" || CheckHome(home) != nil {
			continue
		}
		for _, c := range claims[u.Node] {
			p, id := c.path, c.id
			switch {
			case Under(p, home):
				v.Problem(r, graph.CodeConflict, "spec.home", fmt.Sprintf("%s declares %s on %s, in the home %s of user %s: a home is its user's, who can replace what root puts in it", id, p, u.Node, home, u.Name),
					"put the file outside the home (such as /etc/<app> or /srv/<app>), or give the user another home")
			case Under(home, p):
				v.Problem(r, graph.CodeConflict, "spec.home", fmt.Sprintf("the home %s of user %s is %s, which %s declares as a file on %s", home, u.Name, map[bool]string{true: "that path", false: "under " + p}[home == p], id, u.Node),
					"a path is a file or a directory, not both: change the home or the file's path")
			}
		}
	}
}

// checkCounts refuses more of a kind on one node than mamelet observes (MaxNodeFiles and the like):
// mamelet would refuse the whole linux part of the node's revision.
func checkCounts(g *graph.Graph, v *graph.Validator) {
	most := map[graph.Kind]int{KindFile: MaxNodeFiles, KindUser: MaxNodeUsers, KindAptRepo: MaxNodeAptRepos, KindArtifact: MaxNodeArtifacts,
		KindAptUpgrades: 1}
	seen := map[string]int{}
	for _, r := range g.Sorted() {
		k, ok := kindOf(r.Kind)
		if !ok {
			continue
		}
		node := k.def.Node(r.Ext)
		key := string(r.Kind) + "\x00" + node
		if seen[key]++; seen[key] == most[r.Kind]+1 {
			v.Problem(r, graph.CodeInvalidValue, "spec.node", fmt.Sprintf("more than %d %s are declared on %s; mamelet observes at most %d of them on one node", most[r.Kind], r.Kind, node, most[r.Kind]),
				"spread them over more nodes, or put several files into one (a larger file is a NodeArtifact)")
		}
	}
}

// kindOf is the extension's kind of that name.
func kindOf(k graph.Kind) (kind, bool) {
	for _, d := range kinds {
		if d.def.Kind == k {
			return d, true
		}
	}
	return kind{}, false
}

// graphWarnings are the extension's warnings: the content of a file that looks like a secret, and a
// file that restarts a service the declaration does not have on its node. Then each kind's own.
func graphWarnings(g *graph.Graph) []graph.Warning {
	services := map[string]bool{} // node + "\x00" + unit
	for _, r := range g.Resources {
		if s := r.NodeService; s != nil {
			services[s.Node+"\x00"+strings.TrimSuffix(s.Service, ".service")] = true
		}
	}
	var out []graph.Warning
	for _, r := range g.Sorted() {
		f := FileOf(r)
		if f == nil {
			continue
		}
		if what := secretIn(f.Content); what != "" {
			out = append(out, graph.Warning{Code: CodeSecret, Resource: r.ID(),
				Message: fmt.Sprintf("the content of %s looks like %s: it would be in clear text in the declaration, the plan (and a pull request's comment), the Control Plane's database and the node's revision", f.Path, what),
				Hint:    "keep secrets out of declarations: Mamela has no secret store yet; put the file on the node by other means, or reference a file that the service reads from elsewhere"})
		}
		for _, svc := range f.Restart {
			if !services[f.Node+"\x00"+strings.TrimSuffix(svc, ".service")] {
				out = append(out, graph.Warning{Code: CodeRestartUndeclared, Resource: r.ID(),
					Message: fmt.Sprintf("%s restarts service %s on %s, which the declaration does not have there: it is restarted only if it exists and runs", f.Path, svc, f.Node),
					Hint:    fmt.Sprintf("declare the service: Mamela.service(graph, node, %q, True, True), or check its name", strings.TrimSuffix(svc, ".service"))})
			}
		}
	}
	for _, k := range kinds {
		if k.warnings != nil {
			out = append(out, k.warnings(g)...)
		}
	}
	return out
}

// secrets are what is a secret with high confidence: a private key block, a token of a known form.
// Words such as "password=" are not in it (too many configurations name a password they do not hold).
var secrets = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`), "a private key"},
	{regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK-----`), "a PGP private key"},
	{regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{22,}`), "a GitHub token"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "a Slack token"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "an AWS access key"},
}

// secretIn says what secret content looks like ("" when it does not look like one).
func secretIn(content string) string {
	for _, s := range secrets {
		if s.re.MatchString(content) {
			return s.what
		}
	}
	return ""
}
