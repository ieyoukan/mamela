// Command mamelacd follows a repository of declarations on GitHub (ADR 0010, 0012): it plans each
// pull request of a trusted author and comments the plan, records an approval when an approver
// comments `/mamela approve <digest>`, and applies an approved plan once it is the declaration at
// the head of the branch. It asks the Control Plane's local API (`mamela api`) for all of that,
// and holds only a GitHub token and its own small database: no credential of the infrastructure,
// none of Mamela's state. It never runs explicit operations (delete, reboot, adopt) or imports.
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

const usage = `mamelacd - follow a repository of declarations on GitHub (ADR 0010, 0012)

Usage:
  mamelacd run --config <mamelacd.toml>    read GitHub every interval, until stopped
  mamelacd once --config <mamelacd.toml>   read GitHub once
  mamelacd version

It plans each pull request of a trusted author and comments the plan; an approver approves a plan
by commenting "/mamela approve <digest>"; an approved plan is applied once it is the declaration at
the head of the branch. Planning, approving and applying are the Control Plane's (mamela api).
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
	case "run", "once":
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
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *configPath == "" || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
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
	logger.Printf("mamelacd: following %s (%s), every %s, through %s", e.Config.Repo, e.Config.Branch, e.Config.IntervalOrDefault(), e.Config.MamelaSocket)
	for {
		if err := e.Once(ctx); err != nil {
			logger.Printf("mamelacd: %v", err)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(e.Config.IntervalOrDefault()):
		}
	}
}
