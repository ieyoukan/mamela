// Command mamelet is the Mamela agent that runs in each VM (as a systemd service). It enrolls once
// with the one-time token from cloud-init, then pulls its signed revision, observes, makes only
// the allowed changes and reports, every interval.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/mamelet/config.json", "configuration written by cloud-init")
	once := flag.Bool("once", false, "run one round and exit")
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps

	cfg, err := mamelet.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("mamelet: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	interval := time.Duration(cfg.IntervalSeconds) * time.Second

	for !mamelet.Enrolled(cfg) {
		if err := enroll(ctx, cfg); err != nil {
			log.Printf("mamelet: %v; retrying in %s", err, interval)
			if *once || sleep(ctx, interval) != nil {
				os.Exit(1)
			}
			continue
		}
		log.Printf("mamelet: enrolled as %s (VMID %d)", cfg.Node, cfg.VMID)
	}
	client, err := mamelet.NewClient(cfg)
	if err != nil {
		log.Fatalf("mamelet: %v", err)
	}
	agent := &mamelet.Agent{Config: cfg, CP: client,
		System: &mamelet.System{Runner: mamelet.ExecRunner{}, AllowedOrigins: cfg.AllowedOrigins},
		State:  mamelet.FileState{Path: cfg.StateDir + "/state.json"}, Now: time.Now, Version: version}

	lastErr := ""
	for {
		rep, err := agent.Round(ctx)
		logRound(rep, err, &lastErr)
		if *once {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		if sleep(ctx, interval) != nil {
			return
		}
	}
}

func enroll(ctx context.Context, cfg mamelet.Config) error {
	data, err := os.ReadFile(cfg.EnrollTokenFile)
	if err != nil {
		return fmt.Errorf("not enrolled and no enrollment token: %w", err)
	}
	if err := mamelet.Enroll(ctx, cfg, strings.TrimSpace(string(data))); err != nil {
		return err
	}
	// The token is single-use; it is worthless now, and is removed so it does not linger.
	if err := os.Remove(cfg.EnrollTokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("mamelet: remove the used enrollment token: %v", err)
	}
	return nil
}

// logRound logs what changed and errors, but not every quiet round.
func logRound(rep noderev.Report, err error, lastErr *string) {
	for _, r := range rep.Results {
		if r.Operation == "repair" {
			log.Printf("mamelet: %s: %s", r.Outcome, r.Detail)
		}
	}
	if rep.Refused != "" {
		log.Printf("mamelet: refused the revision: %s", rep.Refused)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != *lastErr {
		if msg == "" {
			log.Printf("mamelet: connected to the Control Plane (revision %d)", rep.Serial)
		} else {
			log.Printf("mamelet: %s", msg)
		}
		*lastErr = msg
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
