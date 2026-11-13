package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/api"
)

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
