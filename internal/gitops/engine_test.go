package gitops

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

type phasedMamela struct {
	st             *store.Store
	now            time.Time
	phase, applies int
	states         []Planned
	g              *graph.Graph
}

func (m *phasedMamela) Plan(ctx context.Context, _ string) (Planned, error) {
	m.now = m.now.Add(time.Second)
	s := state.Empty(m.now)
	p := plan.Build(m.g, s, policy.Default)
	if err := m.st.SavePlan(ctx, m.g, s, policy.Default, p); err != nil {
		return Planned{}, err
	}
	x := m.states[m.phase]
	x.Digest = p.Digest
	x.Revision = m.g.Digest()
	return x, nil
}
func (m *phasedMamela) Revision(context.Context, string) (string, error) { return m.g.Digest(), nil }
func (m *phasedMamela) Approve(ctx context.Context, d, by string, ttl time.Duration) error {
	_, err := m.st.Approve(ctx, d, by, ttl)
	return err
}
func (m *phasedMamela) Apply(context.Context, string) (bool, string, error) {
	m.applies++
	m.phase++
	return true, "phase applied", nil
}

func engineFixture(t *testing.T, states []Planned) (*Engine, *phasedMamela, *fakegithub.GitHub, PR, string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v1","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := &phasedMamela{st: st, now: now, states: states, g: g}
	gh := fakegithub.New("me/infra", "token")
	server := httptest.NewServer(gh)
	t.Cleanup(server.Close)
	sha := strings.Repeat("a", 40)
	work := t.TempDir()
	tree := filepath.Join(work, sha)
	os.MkdirAll(filepath.Join(tree, "infra"), 0700)
	gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: sha})
	e := &Engine{Store: st, Mamela: m, Config: Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, WorkDir: work}, GitHub: &GitHub{API: server.URL, Repo: "me/infra", Token: "token"}}
	pr := PR{Number: 1, MergeCommitSHA: sha}
	return e, m, gh, pr, tree
}

func TestUnresolvedPlansAreNotInSync(t *testing.T) {
	for _, p := range []Planned{{Waiting: 1}, {Blocked: 1}} {
		e, _, gh, pr, tree := engineFixture(t, []Planned{p})
		if err := e.planAndPost(context.Background(), pr, pr.MergeCommitSHA, tree, "infra", true); err != nil {
			t.Fatal(err)
		}
		for _, s := range gh.Statuses(pr.MergeCommitSHA) {
			if s.State == "success" {
				t.Fatalf("unresolved plan reported success: %+v", p)
			}
		}
		for _, c := range gh.Comments(1) {
			if strings.Contains(c.Body, "in sync") {
				t.Fatalf("unresolved plan reported in sync: %+v", p)
			}
		}
	}
}

func TestDependencyPhasesNeedFreshApproval(t *testing.T) {
	ctx := context.Background()
	e, m, gh, pr, tree := engineFixture(t, []Planned{{Changes: 1}, {Waiting: 1}, {Changes: 1}, {}})
	if err := e.planAndPost(ctx, pr, pr.MergeCommitSHA, tree, "infra", true); err != nil {
		t.Fatal(err)
	}
	plans, _ := e.Store.GitopsPlans(ctx, "me/infra", 1)
	if err := m.Approve(ctx, plans[0].Digest, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(ctx, pr, plans[0]); err != nil {
		t.Fatal(err)
	}
	if m.applies != 1 {
		t.Fatal("did not apply first phase")
	}
	statuses := gh.Statuses(pr.MergeCommitSHA)
	if statuses[len(statuses)-1].State != "pending" {
		t.Fatal("waiting phase was marked successful")
	}
	n := len(gh.Comments(1))
	if err := e.merged(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if len(gh.Comments(1)) != n {
		t.Fatal("unchanged wait reposted every poll")
	}
	m.phase = 2 // the next dependency is now observable
	if err := e.merged(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if m.applies != 1 {
		t.Fatal("next phase used the previous approval")
	}
	plans, _ = e.Store.GitopsPlans(ctx, "me/infra", 1)
	if err := m.Approve(ctx, plans[0].Digest, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.merged(ctx, pr); err != nil {
		t.Fatal(err)
	}
	statuses = gh.Statuses(pr.MergeCommitSHA)
	if m.applies != 2 || statuses[len(statuses)-1].State != "success" {
		t.Fatal("final observation did not establish convergence")
	}
	if err := e.merged(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if m.applies != 2 {
		t.Fatal("completed phase ran again")
	}
}
