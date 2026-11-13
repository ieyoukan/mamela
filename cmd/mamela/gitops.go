package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/gitops"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
)

const gitopsUsage = `Usage: mamela gitops <run|once> --config <gitops.toml> [flags]

Follows a repository of declarations on GitHub from the Control Plane (ADR 0010): plans each pull
request of a trusted author and comments the plan; approves a plan when an approver comments
"/mamela approve <digest>"; applies it when the pull request is merged and the merged declaration
is the one approved. It reads GitHub; nothing reaches the Control Plane. Explicit operations
(delete, reboot, adopt) and imports are never done here.

  run    read GitHub every interval, until stopped
  once   read GitHub once

Flags:
`

func runGitops(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" && args[0] != "once" {
		fmt.Fprint(stderr, gitopsUsage)
		return 2
	}
	mode := args[0]
	fs := flag.NewFlagSet("mamela gitops", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "the gitops configuration (TOML)")
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	providersFile := addProvidersFlag(fs)
	policyFile := fs.String("policy", "", "policy JSON file (default: the built-in conservative policy)")
	imageCache := fs.String("image-cache", defaultImageCache(), "directory of downloaded, verified files (images)")
	tf := addToolFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, gitopsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *configPath == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	var cfg gitops.Config
	md, err := toml.DecodeFile(*configPath, &cfg)
	if err != nil {
		return report(stderr, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return report(stderr, fmt.Errorf("%s: unknown keys %v", *configPath, un))
	}
	if err := cfg.Check(); err != nil {
		return report(stderr, fmt.Errorf("%s: %w", *configPath, err))
	}
	tokenPath, err := sitedef.ExpandHome(cfg.TokenFile)
	if err != nil {
		return report(stderr, err)
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return report(stderr, fmt.Errorf("the GitHub token: %w", err))
	}
	api := cfg.API
	if api == "" {
		api = "https://api.github.com"
	}
	t, err := tf.find()
	if err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	tools := []string{"--toolchain", t.dir, "--pome-cache", t.pomeCache}
	m := &cliMamela{st: st, tools: t, common: append([]string{"--db", *db, "--site", *sitePath, "--providers", *providersFile}, tools...),
		policy: *policyFile, imageCache: *imageCache}
	logger := log.New(stderr, "", log.LstdFlags)
	e := &gitops.Engine{Config: cfg, Store: st, Mamela: m, Logf: logger.Printf,
		GitHub: &gitops.GitHub{API: api, Repo: cfg.Repo, Token: strings.TrimSpace(string(token))}}
	if mode == "once" {
		if err := e.Once(ctx); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "gitops: done")
		return 0
	}
	logger.Printf("gitops: following %s (%s), every %s", cfg.Repo, cfg.Branch, cfg.IntervalOrDefault())
	for {
		if err := e.Once(ctx); err != nil {
			logger.Printf("gitops: %v", err)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(cfg.IntervalOrDefault()):
		}
	}
}

// cliMamela does what gitops asks exactly as the CLI does it (the same commands, in process).
type cliMamela struct {
	st         *store.Store
	tools      tools
	common     []string
	policy     string
	imageCache string
}

func (m *cliMamela) run(ctx context.Context, args ...string) (int, string) {
	var out bytes.Buffer
	code := run(ctx, args, &out, &out)
	return code, out.String()
}

func (m *cliMamela) Plan(ctx context.Context, dir string) (gitops.Planned, error) {
	args := append([]string{"plan", "--json"}, m.common...)
	if m.policy != "" {
		args = append(args, "--policy", m.policy)
	}
	var stdout, stderr bytes.Buffer
	code := run(ctx, append(args, dir), &stdout, &stderr)
	out := stdout.String()
	if code != 0 {
		return gitops.Planned{}, errors.New(strings.TrimSpace(out + stderr.String()))
	}
	var output planJSON
	if err := json.Unmarshal([]byte(out), &output, json.RejectUnknownMembers(true)); err != nil {
		return gitops.Planned{}, err
	}
	p, _, err := m.st.LoadPlan(ctx, output.Digest)
	if err != nil {
		return gitops.Planned{}, err
	}
	changes, waiting, blocked := 0, 0, 0
	for _, c := range p.Changes {
		switch {
		case c.Status == plan.StatusBlocked || c.Verdict == policy.Denied:
			blocked++
		case c.Status == plan.StatusWaiting:
			waiting++
		case c.Status == plan.StatusChange:
			changes++
		}
	}
	progs, err := m.st.PlanPrograms(ctx, p.Digest)
	if err != nil {
		return gitops.Planned{}, err
	}
	var names []string
	for _, pr := range progs {
		names = append(names, fmt.Sprintf("%s %s (%s, %s)", pr.Provider, pr.Version, pr.Source, pr.Digest))
	}
	// The text without the local paths of the Control Plane.
	var rendered bytes.Buffer
	plan.Render(&rendered, p)
	text := strings.ReplaceAll(rendered.String(), dir, "<declaration>")
	return gitops.Planned{Digest: p.Digest, Revision: p.Inputs.RevisionDigest, Changes: changes, Waiting: waiting, Blocked: blocked, Text: text, Programs: names}, nil
}

func (m *cliMamela) Revision(ctx context.Context, dir string) (string, error) {
	res, err := m.tools.evaluator().Evaluate(ctx, dir)
	if err != nil {
		return "", err
	}
	return res.Graph.Digest(), nil
}

func (m *cliMamela) Approve(ctx context.Context, digest, by string, ttl time.Duration) error {
	code, out := m.run(ctx, "approve", "--db", m.common[1], "--by", by, "--ttl", ttl.String(), digest)
	if code != 0 {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

func (m *cliMamela) Apply(ctx context.Context, digest string) (bool, string, error) {
	args := append([]string{"apply", "--image-cache", m.imageCache}, m.common...)
	code, out := m.run(ctx, append(args, digest)...)
	return code == 0, out, nil
}
