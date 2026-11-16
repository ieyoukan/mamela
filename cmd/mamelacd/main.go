// Command mamelacd follows a repository of declarations on GitHub (ADR 0012; docs/gitops.md). See
// package internal/gitops.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/gitops"
	"github.com/ieyoukan/mamela/internal/version"
)

const usage = `mamelacd - follow a repository of declarations on GitHub (ADR 0012)

Usage:
  mamelacd run --config <mamelacd.toml>      read GitHub every interval, until stopped
  mamelacd once --config <mamelacd.toml>     read GitHub once
  mamelacd status --config <mamelacd.toml>   what it is doing: the last poll, the next, each declaration
                                             (exit 1 when it looks stopped, or its polls keep failing)
  mamelacd repin --config <mamelacd.toml> --login <login>
                                             trust the GitHub account the login has now (after a rename)
  mamelacd version

It comments the plan of each pull request of a trusted author. An approver approves a plan by
commenting "/mamela approve <digest>". An approved plan is applied once it is the declaration at
the head of the branch. Planning, approving and applying are the Control Plane's (mamela api).

A SIGHUP makes "run" read the token file again (systemctl kill -s HUP mamelacd); run "status" as the
user mamelacd runs as.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run", "once", "status", "repin":
	case "version", "--version":
		fmt.Fprintf(stdout, "mamelacd %s (%s)\n", version.Current(), api.Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mamelacd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	mode := args[0]
	fs := flag.NewFlagSet("mamelacd "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "the configuration (TOML)")
	var login *string
	if mode == "repin" {
		login = fs.String("login", "", "the GitHub login whose pinned account is forgotten")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *configPath == "" || fs.NArg() != 0 || (login != nil && *login == "") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch mode {
	case "status", "repin":
		cfg, err := gitops.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "mamelacd: %v\n", err)
			return 1
		}
		if mode == "repin" {
			if err := gitops.Repin(ctx, cfg, *login, stdout); err != nil {
				fmt.Fprintf(stderr, "mamelacd: %v\n", err)
				return 1
			}
			return 0
		}
		healthy, err := gitops.Status(ctx, cfg, time.Now(), stdout)
		if err != nil {
			fmt.Fprintf(stderr, "mamelacd: %v\n", err)
			return 1
		}
		if !healthy {
			return 1
		}
		return 0
	}
	// A SIGHUP is a request to read the token file again (a rotated token), not to stop. Catching
	// it before anything else keeps it from ending the process.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	logger := log.New(stderr, "", log.LstdFlags)
	e, closeEngine, err := gitops.Open(ctx, *configPath, logger.Printf)
	if err != nil {
		fmt.Fprintf(stderr, "mamelacd: %v\n", err)
		return 1
	}
	defer closeEngine()
	if mode == "once" {
		if err := e.Once(ctx); err != nil {
			fmt.Fprintf(stderr, "mamelacd: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "mamelacd: done")
		return 0
	}
	logger.Printf("mamelacd: following %s (%s), every %s (%s while something is under way), through %s; `mamelacd status --config %s` says how it is doing",
		e.Config.Repo, e.Config.Branch, e.Config.IntervalOrDefault(), e.Config.ActiveIntervalOrDefault(), e.Config.MamelaSocket, *configPath)
	reload := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-hup:
				select {
				case reload <- struct{}{}:
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	e.Run(ctx, reload)
	return 0
}
