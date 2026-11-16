package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/api"
)

// Config is mamelacd's configuration (TOML; docs/gitops.md).
type Config struct {
	// Repo is the GitHub repository of the declarations ("owner/name"), Branch the branch whose
	// head is applied, Dirs the declarations (Pome directories) in it.
	Repo   string   `toml:"repo"`
	Branch string   `toml:"branch"`
	Dirs   []string `toml:"dirs"`
	// API is GitHub's API (default https://api.github.com); TokenFile holds a token limited to
	// the repository: contents read, pull requests and commit statuses read and write.
	API       string `toml:"api_url"`
	TokenFile string `toml:"token_file"`
	// Sync says what makes a head of the branch applied: "approval" (the default: a pull request's
	// plan, approved by an approver's comment) or "push" (ADR 0014: the head itself, from an author,
	// is the approval; a plan that deletes still needs the Control Plane's host).
	Sync string `toml:"sync"`
	// Authors are whose pull requests are planned (with sync = "push": whose commits are applied);
	// Approvers who may approve (GitHub logins). SelfApproval lets an author approve their own
	// pull request's plan.
	Authors      []string `toml:"authors"`
	Approvers    []string `toml:"approvers"`
	SelfApproval *bool    `toml:"self_approval"`
	// ApprovalTTL is how long an approval lasts (default 24h); Interval how often GitHub is
	// read (default 1m).
	ApprovalTTL string `toml:"approval_ttl"`
	Interval    string `toml:"interval"`
	// Resync is how often a declaration at the head that is in sync is planned again, to find drift
	// (the world changed under it: a service stopped, a binary replaced): default 10m, "0" never.
	// A drift is shown on the head's commit status. SelfHeal (only with sync = "push") approves the
	// plan that undoes the drift, as the head's author; without it, a drift waits for an approval
	// on the Control Plane's host. Planning only observes: nothing changes by itself.
	Resync   string `toml:"resync"`
	SelfHeal bool   `toml:"self_heal"`
	// ActiveInterval is how often GitHub is read while something is under way (a declaration waits
	// for what it depends on: a machine, an agent's report): default 10s, 5s or more, never more
	// than Interval. Idle, Interval applies.
	ActiveInterval string `toml:"active_interval"`
	// Retry is how long after a failed apply the head is planned (and, with sync = "push",
	// approved and applied) again, up to RetryMax times (default 3; 0 never): default 5m, 1m or
	// more, "0" never. A failed apply is safe to repeat: it observes first and continues. A new
	// commit starts over. Retry is also how long after the evaluator was unavailable (not the
	// declaration's fault) the declaration is evaluated again, with no limit; RetryMax also limits how
	// many times in a row the same change may come back (stale, or applied and still planned) before
	// a push stops approving it (MAM-GIT-0101).
	Retry    string `toml:"retry"`
	RetryMax *int   `toml:"retry_max"`
	// MamelaSocket is the Control Plane's local API (`mamela api`); StateDB mamelacd's own database.
	MamelaSocket string `toml:"mamela_socket"`
	StateDB      string `toml:"state_db"`
}

// The values of Config.Sync.
const (
	SyncApproval = "approval"
	SyncPush     = "push"
)

// PushSync says whether a head of the branch from an author is applied without an approval comment.
func (c Config) PushSync() bool { return c.Sync == SyncPush }

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Check says what is wrong with the configuration.
func (c Config) Check() error {
	switch {
	case !repoRe.MatchString(c.Repo):
		return fmt.Errorf("repo %q: owner/name", c.Repo)
	case c.Branch == "" || len(c.Dirs) == 0:
		return fmt.Errorf("branch and dirs are required")
	case c.TokenFile == "":
		return fmt.Errorf("token_file is required (a token limited to %s)", c.Repo)
	case c.Sync != "" && c.Sync != SyncApproval && c.Sync != SyncPush:
		return fmt.Errorf("sync %q: %q or %q", c.Sync, SyncApproval, SyncPush)
	case len(c.Authors) == 0 || (len(c.Approvers) == 0 && !c.PushSync()):
		return fmt.Errorf("authors and approvers are required: whose pull requests are planned, and who approves (approvers may be empty with sync = %q)", SyncPush)
	case c.SelfHeal && !c.PushSync():
		return fmt.Errorf("self_heal needs sync = %q (the author's push is what approves)", SyncPush)
	case c.MamelaSocket == "":
		return fmt.Errorf("mamela_socket is required (the Control Plane's local API, `mamela api --socket`)")
	case c.StateDB == "":
		return fmt.Errorf("state_db is required (mamelacd's own database)")
	}
	for _, d := range c.Dirs {
		if !localDir(d) {
			return fmt.Errorf("dir %q: a directory in the repository", d)
		}
	}
	if _, err := c.ttl(); err != nil {
		return err
	}
	if _, err := c.interval(); err != nil {
		return err
	}
	if _, err := c.resync(); err != nil {
		return err
	}
	if _, err := c.activeInterval(); err != nil {
		return err
	}
	if _, err := c.retry(); err != nil {
		return err
	}
	if c.RetryMax != nil && (*c.RetryMax < 0 || *c.RetryMax > 20) {
		return fmt.Errorf("retry_max %d: 0 to 20", *c.RetryMax)
	}
	return nil
}

// activeInterval is how often GitHub is read while something is under way.
func (c Config) activeInterval() (time.Duration, error) {
	iv, err := c.interval()
	if err != nil {
		return 0, err
	}
	if c.ActiveInterval == "" {
		return min(10*time.Second, iv), nil
	}
	d, err := time.ParseDuration(c.ActiveInterval)
	if err != nil || d < 5*time.Second || d > iv {
		return 0, fmt.Errorf("active_interval %q: 5s or more, and not more than interval (%s)", c.ActiveInterval, iv)
	}
	return d, nil
}

// ActiveIntervalOrDefault is how often GitHub is read while something is under way.
func (c Config) ActiveIntervalOrDefault() time.Duration {
	d, _ := c.activeInterval()
	return d
}

// retry is how long after a failed apply the head is tried again (0: never).
func (c Config) retry() (time.Duration, error) {
	if c.Retry == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.Retry)
	if err != nil || (d != 0 && d < time.Minute) {
		return 0, fmt.Errorf("retry %q: 1m or more, or 0 for never", c.Retry)
	}
	return d, nil
}

// retryMax is how many times a failed head is tried again.
func (c Config) retryMax() int {
	if c.RetryMax == nil {
		return 3
	}
	return *c.RetryMax
}

// resync is how often drift is looked for (0: never).
func (c Config) resync() (time.Duration, error) {
	if c.Resync == "" {
		return 10 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.Resync)
	if err != nil || (d != 0 && d < time.Minute) {
		return 0, fmt.Errorf("resync %q: 1m or more, or 0 for never", c.Resync)
	}
	return d, nil
}

func localDir(d string) bool {
	return d != "" && !path.IsAbs(d) && !strings.Contains(d, "\\") && path.Clean(d) == d && d != "." && d != ".." && !strings.HasPrefix(d, "../")
}

func (c Config) ttl() (time.Duration, error) {
	if c.ApprovalTTL == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(c.ApprovalTTL)
	if err != nil || d <= 0 || d > api.MaxTTL {
		return 0, fmt.Errorf("approval_ttl %q: a duration up to %s", c.ApprovalTTL, api.MaxTTL)
	}
	return d, nil
}

func (c Config) interval() (time.Duration, error) {
	if c.Interval == "" {
		return time.Minute, nil
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil || d < 10*time.Second {
		return 0, fmt.Errorf("interval %q: 10s or more", c.Interval)
	}
	return d, nil
}

// IntervalOrDefault is how often GitHub is read.
func (c Config) IntervalOrDefault() time.Duration {
	d, _ := c.interval()
	return d
}

// LoadConfig reads and checks the configuration. It does not read the token or reach anything.
func LoadConfig(configPath string) (Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(configPath, &cfg)
	if err != nil {
		return cfg, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		return cfg, fmt.Errorf("%s: unknown keys %v", configPath, un)
	}
	if err := cfg.Check(); err != nil {
		return cfg, fmt.Errorf("%s: %w", configPath, err)
	}
	return cfg, nil
}

// Open reads the configuration and the GitHub token, opens mamelacd's database, and checks that
// the Control Plane's API answers in this version.
func Open(ctx context.Context, configPath string, logf func(string, ...any)) (*Engine, func(), error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("the GitHub token: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, nil, errors.New("the GitHub token is empty")
	}
	ghAPI := cfg.API
	if ghAPI == "" {
		ghAPI = "https://api.github.com"
	}
	client := api.NewClient(cfg.MamelaSocket)
	if _, err := client.Info(ctx); err != nil {
		return nil, nil, err
	}
	st, err := OpenStore(ctx, cfg.StateDB, time.Now)
	if err != nil {
		return nil, nil, err
	}
	e := &Engine{Config: cfg, Store: st, Mamela: client, Logf: logf,
		GitHub: &GitHub{API: ghAPI, Repo: cfg.Repo, Token: strings.TrimSpace(string(token)), TokenFile: cfg.TokenFile}}
	return e, func() { st.Close() }, nil
}
