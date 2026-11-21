package plan_test

// The golden tests fix, byte for byte, what Mamela makes of k3s and of the nodes: the plans (their
// canonical JSON, which the approval's digest covers), the graphs' canonical form and digest, the
// revisions the Control Plane publishes, what mamelet reports and runs, the node provider's manifest
// (an input of ProvidersDigest), every operation's decision and the programs mamelet may run.
//
// They were written before k3s was moved out of the core (docs/design/node-extensions.md), and the
// move must not change one byte of them: a plan approved before the move is the same plan after it.
// Regenerate them only for a change that means to change what they fix:
//
//	go test ./internal/plan -run Golden -update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const goldenDir = "testdata/golden"

// golden compares got with the file, or writes it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with -update to write it)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file:\n got: %s\nwant: %s", name, clip(got), clip(want))
	}
}

func clip(b []byte) string {
	if len(b) > 2000 {
		return string(b[:2000]) + "..."
	}
	return string(b)
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Each scenario is a declaration (graph.json), an observation (snapshot.json) and a policy: the
// plan made of them is fixed, with its digest, how it is shown, and what applying it would use.
func TestGoldenPlans(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join(goldenDir, "plan"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 30 {
		t.Fatalf("only %d plan scenarios", len(dirs))
	}
	for _, d := range dirs {
		t.Run(d.Name(), func(t *testing.T) {
			dir := filepath.Join(goldenDir, "plan", d.Name())
			in := read(t, filepath.Join(dir, "graph.json"))
			g, err := graph.Decode(in)
			if err != nil {
				t.Fatal(err)
			}
			if got := append(g.Canonical(), '\n'); !bytes.Equal(got, in) {
				t.Errorf("the graph's canonical form changed:\n got: %s\nwant: %s", got, in)
			}
			snap, err := state.Decode(read(t, filepath.Join(dir, "snapshot.json")))
			if err != nil {
				t.Fatal(err)
			}
			pol, err := policy.Decode(read(t, filepath.Join(dir, "policy.json")))
			if err != nil {
				t.Fatal(err)
			}
			providers := spec.Providers{node.Manifest()}.Digest()
			p := plan.BuildFor(g, snap, pol, providers)
			name := "plan/" + d.Name() + "/"
			golden(t, name+"plan.json", append(p.Canonical(), '\n'))
			golden(t, name+"digest.txt", []byte("plan "+p.Digest+"\ngraph "+g.Digest()+"\nsnapshot "+snap.Digest()+"\n"))
			var r bytes.Buffer
			plan.Render(&r, p)
			golden(t, name+"render.txt", r.Bytes())
			golden(t, name+"requirements.json", jsonOf(t, (&site.Site{}).PlannedRequirements(p, g, pol)))
			if q := plan.BuildFor(g, snap, pol, providers); q.Digest != p.Digest {
				t.Error("the plan is not deterministic")
			}
		})
	}
}

// Declarations as written (out of order, repeated, wrong): their canonical form and digest, their
// warnings, or every problem found.
func TestGoldenGraphs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "graph", "*.json"))
	if err != nil || len(files) < 3 {
		t.Fatalf("graph inputs: %v %v", files, err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			g, err := graph.Decode(read(t, f))
			if err != nil {
				var ge *graph.Error
				if !errors.As(err, &ge) {
					t.Fatal(err)
				}
				out.WriteString("problems:\n")
				for _, p := range ge.Problems {
					fmt.Fprintf(&out, "%s | %s | %s | %s\n", p.Code, p.Path, p.Message, p.Hint)
				}
				out.WriteString("text:\n" + err.Error() + "\n")
			} else {
				fmt.Fprintf(&out, "canonical:\n%s\ndigest %s\nwarnings:\n", g.Canonical(), g.Digest())
				for _, w := range g.Warnings() {
					fmt.Fprintf(&out, "%s | %s | %s | %s\n", w.Code, w.Resource, w.Message, w.Hint)
				}
				out.WriteString("nodes:\n")
				for _, r := range g.Sorted() {
					fmt.Fprintf(&out, "%s -> %q builtin=%t\n", r.ID(), r.Node(), graph.Builtin(r.Kind))
				}
			}
			golden(t, "graph/"+name+".out", out.Bytes())
		})
	}
}

// Every operation's decision: verdict, code, message and hint (the plan's JSON has them all).
func TestGoldenDecisions(t *testing.T) {
	pol := policy.Default
	pol.AutoStartServices = []string{"k3s"}
	type row struct {
		Subject  policy.Subject  `json:"subject"`
		Decision policy.Decision `json:"decision"`
	}
	var rows []row
	for _, op := range []string{"change-foreign", "install-package", "enable-service", "disable-service", "start-service", "stop-service",
		"configure-service-repair", "install-k3s", "upgrade-k3s", "write-k3s-manifest", "remove-k3s-node", "update-mamelet", "no-such-op", ""} {
		for _, s := range []policy.Subject{{Op: policy.Op(op)}, {Op: policy.Op(op), Service: "k3s"},
			{Op: policy.Op(op), Removes: true}, {Op: policy.Op(op), Origins: []string{"Ubuntu:24.04/noble", "Other:x"}}} {
			rows = append(rows, row{s, pol.Decide(s)})
		}
	}
	rows = append(rows, row{policy.Subject{Op: "protected"}, policy.ProtectedDecision})
	rows = append(rows, row{policy.Subject{Op: "observe-only"}, policy.ObserveOnlyDecision})
	golden(t, "decisions.json", jsonOf(t, rows))
}

// The node provider's manifest is an input of every plan's ProvidersDigest: a change voids every
// approval.
func TestGoldenNodeManifest(t *testing.T) {
	m := node.Manifest()
	golden(t, "node-manifest.json", jsonOf(t, m))
	golden(t, "providers-digest.txt", []byte(spec.Providers{m}.Digest()+"\n"))
}

// The programs mamelet runs, and nothing else.
func TestGoldenMameletPrograms(t *testing.T) {
	golden(t, "mamelet-programs.txt", []byte(strings.Join(mamelet.Programs(), "\n")+"\n"))
}

var goldenT0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const goldenSHA = "265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda"

func goldenGraph(t *testing.T, js string) *graph.Graph {
	t.Helper()
	g, err := graph.Decode([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func goldenVM(name string) string {
	return `{"kind":"TestVM","name":"` + name + `","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}}`
}

func goldenRes(kind, name, spec string) string {
	return `{"kind":"` + kind + `","name":"` + name + `","spec":` + spec + `}`
}

// The revisions the Control Plane publishes from declarations (Publish, while planning) and takes
// away from them (Prune, when a plan is applied): their content, with the join token named for its
// cluster (it is random).
func TestGoldenRevisions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return goldenT0 })
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	full := goldenGraph(t, `{"schema":"mamela-graph/v2","resources":[`+strings.Join([]string{
		goldenVM("srv"), goldenVM("a1"), goldenVM("a2"),
		goldenRes("NodeK3s", "srv/k3s", `{"node":"srv","role":"server","sha256":"`+goldenSHA+`","version":"v1.37.2+k3s1"}`),
		goldenRes("NodeK3s", "a1/k3s", `{"node":"a1","role":"agent","server":"srv","sha256":"`+goldenSHA+`","version":"v1.37.2+k3s1"}`),
		goldenRes("NodeK3s", "a2/k3s", `{"node":"a2","role":"agent","server":"srv","sha256":"`+goldenSHA+`","version":"v1.37.2+k3s1"}`),
		goldenRes("NodeService", "srv/k3s", `{"enabled":true,"node":"srv","running":true,"service":"k3s"}`),
		goldenRes("NodeService", "a1/k3s", `{"enabled":true,"node":"a1","running":true,"service":"k3s"}`),
		goldenRes("NodePackage", "srv/curl", `{"node":"srv","package":"curl","state":"Present"}`),
		goldenRes("NodeK3sManifest", "srv/manifest/zeta", `{"node":"srv","name":"zeta","content":"kind: ConfigMap\n"}`),
		goldenRes("NodeK3sManifest", "srv/manifest/alpha", `{"node":"srv","name":"alpha","content":"kind: Namespace\n"}`),
		goldenRes("NodeMamelet", "a1/mamelet", `{"node":"a1","sha256":"`+goldenSHA+`","version":"0.3.2"}`),
	}, ",")+`]}`)
	ids := map[string]int64{"srv": 7001, "a1": 7002, "a2": 7003}
	var out bytes.Buffer
	tokens := map[string]string{}
	for _, cluster := range []string{"srv"} {
		tok, err := st.K3sToken(ctx, cluster)
		if err != nil {
			t.Fatal(err)
		}
		tokens[tok] = "<join token of cluster " + cluster + ">"
	}
	dump := func(step string) {
		fmt.Fprintf(&out, "== %s\n", step)
		for _, n := range []string{"srv", "a1", "a2"} {
			r, err := st.NodeRevision(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if r == nil {
				fmt.Fprintf(&out, "%s: none\n", n)
				continue
			}
			c := *r
			if c.K3s != nil {
				k := *c.K3s
				if name, ok := tokens[k.Token]; ok {
					k.Token = name
				} else if k.Token != "" {
					k.Token = "<another token>"
				}
				c.K3s = &k
			}
			fmt.Fprintf(&out, "%s: %s\n", n, c.Content())
		}
	}
	pol := policy.Default
	// 1. Before the server reports its address: the agents have no k3s in their revisions yet.
	if err := node.Publish(ctx, st, full, pol, "", ids); err != nil {
		t.Fatal(err)
	}
	dump("publish, the server has not reported")
	// 2. The server reports its addresses: the agents join the first.
	if err := st.PutNodeReport(ctx, noderev.Report{Node: "srv", VMID: 7001, Packages: []noderev.PackageState{}, Services: []noderev.ServiceState{}, Results: []noderev.Result{},
		K3s: &noderev.K3sState{Addresses: []string{"192.0.2.77", "192.0.2.12"}}}); err != nil {
		t.Fatal(err)
	}
	if err := node.Publish(ctx, st, full, pol, "", ids); err != nil {
		t.Fatal(err)
	}
	dump("publish, the server reported its addresses")
	// 3. Grants of every kind, then a publish: an old removal of a node that is declared again is
	// taken away; on a protected node, every grant is.
	exp := goldenT0.Add(time.Hour)
	once := func(op, action string) noderev.Once {
		return noderev.Once{Operation: "op-" + op, Attempt: 1, Action: action, Expires: exp}
	}
	grant := func(n string, f func(r *noderev.Revision)) {
		if _, err := st.PublishNodeRevision(ctx, n, func(prev *noderev.Revision) (noderev.Revision, error) {
			r := *prev
			r.Packages, r.Services, r.Manifests = slices.Clone(prev.Packages), slices.Clone(prev.Services), slices.Clone(prev.Manifests)
			f(&r)
			return r, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	grant("srv", func(r *noderev.Revision) {
		k := *r.K3s
		in := once("aaaaaaaaaaaaaaaaaaaaaaaa", noderev.ActionInstall)
		k.Install = &in
		k.Remove = &noderev.NodeRemoval{Node: "a2", Once: once("bbbbbbbbbbbbbbbbbbbbbbbb", noderev.ActionRemove)}
		r.K3s = &k
		w := once("cccccccccccccccccccccccc", noderev.ActionWrite)
		r.Manifests[0].Write = &w
		r.Packages[0].Install = &noderev.Install{Operation: "op-dddddddddddddddddddddddd", Attempt: 1, ChangeSet: "sha256:" + goldenSHA, Expires: exp}
		r.Services[0].Repair = []string{noderev.ActionStart}
		r.Services[0].Running = true
	})
	grant("a1", func(r *noderev.Revision) {
		k := *r.K3s
		in := once("eeeeeeeeeeeeeeeeeeeeeeee", noderev.ActionInstall)
		k.Install = &in
		r.K3s = &k
		r.Mamelet = &noderev.MameletUpdate{SHA256: goldenSHA, Once: once("ffffffffffffffffffffffff", noderev.ActionUpdate)}
		r.Services[0].Once = []noderev.Once{once("abababababababababababab", noderev.ActionStart)}
	})
	dump("grants")
	if err := node.Publish(ctx, st, full, pol, "", ids); err != nil {
		t.Fatal(err)
	}
	dump("publish again: a2 is declared, so its removal is taken away")
	protected := pol
	protected.ProtectedNodes = []string{"a1"}
	if err := node.Publish(ctx, st, full, protected, "", ids); err != nil {
		t.Fatal(err)
	}
	dump("publish with a1 protected")
	// 4. Publishing a declaration that has less takes nothing away; pruning with it does.
	less := goldenGraph(t, `{"schema":"mamela-graph/v2","resources":[`+strings.Join([]string{
		goldenVM("srv"), goldenVM("a1"), goldenVM("a2"),
		goldenRes("NodeK3s", "srv/k3s", `{"node":"srv","role":"server","sha256":"`+goldenSHA+`","version":"v1.37.2+k3s1"}`),
		goldenRes("NodeK3s", "a2/k3s", `{"node":"a2","role":"agent","server":"srv","sha256":"`+goldenSHA+`","version":"v1.37.2+k3s1"}`),
		goldenRes("NodeK3sManifest", "srv/manifest/alpha", `{"node":"srv","name":"alpha","content":"kind: Namespace\n"}`),
	}, ",")+`]}`)
	if err := node.Publish(ctx, st, less, pol, "", ids); err != nil {
		t.Fatal(err)
	}
	dump("publish a declaration that has less")
	changed, err := node.Prune(ctx, st, less)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&out, "pruned: %v\n", changed)
	dump("prune")
	golden(t, "revisions.txt", out.Bytes())
}

// goldenOS is a node as mamelet sees it: apt, systemd and k3s's kubectl over a few fields. It
// records every program mamelet runs.
type goldenOS struct {
	installed map[string]bool
	services  map[string][3]bool // exists, enabled, running
	nodes     []string           // kubectl get nodes lines
	commands  []string
}

const goldenSim = "Inst curl (8.5.0-2ubuntu10.6 Ubuntu:24.04/noble-updates [amd64])\n"

func (o *goldenOS) Run(_ context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	o.commands = append(o.commands, strings.TrimSpace(strings.Join(env, " ")+" "+program+" "+strings.Join(args, " ")))
	last := args[len(args)-1]
	switch {
	case strings.HasSuffix(program, "dpkg-query"):
		if o.installed[last] {
			return []byte("installed\t8.5.0-2ubuntu10.6\n"), nil, 0, nil
		}
		return nil, []byte("dpkg-query: no packages found matching " + last), 1, nil
	case strings.HasSuffix(program, "apt-get"):
		if slices.Contains(args, "-s") {
			return []byte(goldenSim), nil, 0, nil
		}
		o.installed[last] = true
		return nil, nil, 0, nil
	case strings.HasSuffix(program, "systemctl"):
		s := o.services[last]
		switch args[0] {
		case "show":
			if slices.Contains(args, "--timestamp=unix") {
				state := "inactive"
				if s[2] {
					state = "active"
				}
				return []byte("ActiveState=" + state + "\nExecMainStartTimestamp=@0\n"), nil, 0, nil
			}
			props := func(b bool, y, n string) string {
				if b {
					return y
				}
				return n
			}
			return []byte("LoadState=" + props(s[0], "loaded", "not-found") + "\nUnitFileState=" + props(s[1], "enabled", "disabled") +
				"\nActiveState=" + props(s[2], "active", "inactive") + "\n"), nil, 0, nil
		case "enable":
			s[1] = true
		case "start", "restart":
			s[2] = true
		}
		o.services[last] = s
		return nil, nil, 0, nil
	case strings.HasSuffix(program, "/k3s"):
		switch args[1] {
		case "get":
			return []byte(strings.Join(o.nodes, "\n") + "\n"), nil, 0, nil
		case "delete":
			o.nodes = slices.DeleteFunc(o.nodes, func(l string) bool { return strings.HasPrefix(l, last+"\t") })
		}
		return nil, nil, 0, nil
	}
	return nil, []byte("not on this node"), 127, nil
}

type goldenCP struct {
	signed  noderev.Signed
	reports []noderev.Report
}

func (c *goldenCP) Revision(context.Context) (noderev.Signed, error) { return c.signed, nil }
func (c *goldenCP) Report(_ context.Context, r noderev.Report) error {
	c.reports = append(c.reports, r)
	return nil
}
func (c *goldenCP) Mamelet(context.Context, string) ([]byte, error) {
	return []byte("not the approved binary"), nil
}

type goldenState struct{ st mamelet.State }

func (s *goldenState) Load() (mamelet.State, error) {
	if s.st.Attempts == nil {
		s.st.Attempts = map[string]noderev.Result{}
	}
	return s.st, nil
}
func (s *goldenState) Save(st mamelet.State) error { s.st = st; return nil }

// What mamelet does with a revision that grants one of everything, in what order, and what it
// reports; and what the Control Plane observes from that report.
func TestGoldenMameletRound(t *testing.T) {
	ctx := context.Background()
	binary := []byte("\x7fELF k3s golden")
	sum := sha256.Sum256(binary)
	k3sSHA := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(binary) }))
	defer srv.Close()

	changes, err := mamelet.ParseSimulation(goldenSim)
	if err != nil {
		t.Fatal(err)
	}
	cs := noderev.NewChangeSet(changes)
	once := func(op, action string) noderev.Once {
		return noderev.Once{Operation: "op-" + op, Attempt: 1, Action: action}
	}
	in, w, rm := once("aaaaaaaaaaaaaaaaaaaaaaaa", noderev.ActionInstall), once("cccccccccccccccccccccccc", noderev.ActionWrite), once("bbbbbbbbbbbbbbbbbbbbbbbb", noderev.ActionRemove)
	rev := noderev.Revision{Schema: noderev.Schema, Node: "srv", VMID: 7001, Serial: 3, IssuedAt: goldenT0, ExpiresAt: goldenT0.Add(time.Hour),
		Packages: []noderev.Package{{Name: "curl", Install: &noderev.Install{Operation: "op-dddddddddddddddddddddddd", Attempt: 1, ChangeSet: cs.Digest}}},
		Services: []noderev.Service{
			{Name: "k3s", Enabled: true, Running: true, Repair: []string{}, Once: []noderev.Once{once("abababababababababababab", noderev.ActionEnable), once("acacacacacacacacacacacac", noderev.ActionStart)}},
			{Name: "nginx", Enabled: false, Running: true, Repair: []string{noderev.ActionStart}, Once: []noderev.Once{}},
		},
		K3s:       &noderev.K3s{Version: "v1.37.2+k3s1", SHA256: k3sSHA, Role: noderev.K3sServer, Token: strings.Repeat("7e", 32), Install: &in, Remove: &noderev.NodeRemoval{Node: "old", Once: rm}},
		Manifests: []noderev.Manifest{{Name: "app", Content: "kind: ConfigMap\n", Write: &w}, {Name: "kept", Content: "kind: Secret\n"}},
		Mamelet:   &noderev.MameletUpdate{SHA256: strings.Repeat("ab", 32), Once: once("efefefefefefefefefefefef", noderev.ActionUpdate)},
	}
	pub, key, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := noderev.Sign(key, rev)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	self := filepath.Join(root, "self")
	if err := os.WriteFile(self, []byte("mamelet golden"), 0o755); err != nil {
		t.Fatal(err)
	}
	osys := &goldenOS{installed: map[string]bool{}, services: map[string][3]bool{"k3s": {true, false, false}, "nginx": {true, false, false}},
		nodes: []string{"srv\tv1.37.2+k3s1\tTrue", "old\tv1.37.1+k3s1\tFalse"}}
	cp := &goldenCP{signed: signed}
	a := &mamelet.Agent{Config: mamelet.Config{Node: "srv", VMID: 7001, RevisionKey: pub}, CP: cp, State: &goldenState{}, Version: "golden",
		Now: func() time.Time { return goldenT0.Add(time.Minute) },
		System: &mamelet.System{Runner: osys, AllowedOrigins: []string{"Ubuntu:"}, Root: root, HTTP: srv.Client(), K3sReleases: srv.URL, SelfPath: self,
			Addresses: func() []string { return []string{"192.0.2.12", "192.0.2.77"} }}}
	var out bytes.Buffer
	for i := 1; i <= 2; i++ {
		osys.commands = nil
		rep, err := a.Round(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "== round %d\ncommands:\n%s\nreport:\n%s", i, strings.Join(osys.commands, "\n"), jsonOf(t, rep))
	}
	golden(t, "mamelet-round.txt", out.Bytes())

	// What the Control Plane observes from the last report.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return goldenT0 })
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stored := rev
	stored.Packages, stored.Services = []noderev.Package{{Name: "curl"}}, []noderev.Service{{Name: "k3s", Enabled: true, Running: true}, {Name: "nginx", Running: true, Repair: []string{noderev.ActionStart}}}
	stored.K3s = &noderev.K3s{Version: "v1.37.2+k3s1", SHA256: k3sSHA, Role: noderev.K3sServer, Token: strings.Repeat("7e", 32)}
	stored.Manifests, stored.Mamelet = []noderev.Manifest{{Name: "app", Content: "kind: ConfigMap\n"}, {Name: "kept", Content: "kind: Secret\n"}}, nil
	if _, err := st.PublishNodeRevision(ctx, "srv", func(*noderev.Revision) (noderev.Revision, error) { return stored, nil }); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNodeReport(ctx, cp.reports[len(cp.reports)-1]); err != nil {
		t.Fatal(err)
	}
	snap, err := (&node.Provider{Store: st, Clock: func() time.Time { return goldenT0 }}).Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "observed.json", append(snap.Canonical(), '\n'))
}
