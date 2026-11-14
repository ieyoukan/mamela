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
	// Authors are whose pull requests are planned; Approvers who may approve (GitHub logins).
	// SelfApproval lets an author approve their own pull request's plan.
	Authors      []string `toml:"authors"`
	Approvers    []string `toml:"approvers"`
	SelfApproval *bool    `toml:"self_approval"`
	// ApprovalTTL is how long an approval lasts (default 24h); Interval how often GitHub is
	// read (default 1m).
	ApprovalTTL string `toml:"approval_ttl"`
	Interval    string `toml:"interval"`
	// MamelaSocket is the Control Plane's local API (`mamela api`); StateDB mamelacd's own database.
	MamelaSocket string `toml:"mamela_socket"`
	StateDB      string `toml:"state_db"`
}

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
	case len(c.Authors) == 0 || len(c.Approvers) == 0:
		return fmt.Errorf("authors and approvers are required: whose pull requests are planned, and who approves")
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
	_, err := c.interval()
	return err
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

// Open reads the configuration and the GitHub token, opens mamelacd's database, and checks that
// the Control Plane's API answers in this version.
func Open(ctx context.Context, configPath string, logf func(string, ...any)) (*Engine, func(), error) {
	var cfg Config
	md, err := toml.DecodeFile(configPath, &cfg)
	if err != nil {
		return nil, nil, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, nil, fmt.Errorf("%s: unknown keys %v", configPath, un)
	}
	if err := cfg.Check(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", configPath, err)
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
		GitHub: &GitHub{API: ghAPI, Repo: cfg.Repo, Token: strings.TrimSpace(string(token))}}
	return e, func() { st.Close() }, nil
}
