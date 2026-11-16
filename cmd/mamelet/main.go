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
	showVersion := flag.Bool("version", false, "print the version and exit (an update runs the new binary with this before it replaces mamelet)")
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps
	if *showVersion {
		fmt.Println("mamelet", version)
		return
	}

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

	// An update of mamelet that has not proved itself: count this start, and go back to the previous
	// binary if the new one keeps failing (update.go).
	if back, err := agent.Boot(); err != nil {
		log.Printf("mamelet: update state: %v", err)
	} else if back {
		rollBack(agent, "the new mamelet failed to start or to report in time")
	}

	lastErr, certMsg := "", ""
	var seen int64 // the newest revision serial the Control Plane has told of
	for {
		rep, err := agent.Round(ctx)
		logRound(rep, err, &lastErr)
		keepCertificate(ctx, cfg, client, time.Now(), &certMsg)
		if agent.Restarting() {
			restart(agent)
		}
		if err != nil && agent.UpdateOverdue() {
			rollBack(agent, "the new mamelet did not report in time")
		}
		if *once {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		// The next round comes as soon as the Control Plane has a newer revision for this node (an
		// approved operation), or after the interval. When the round failed, the interval.
		var w mamelet.Waiter
		if err == nil {
			w = client
		}
		if seen, err = mamelet.Pause(ctx, w, max(seen, rep.Serial), interval, time.Now, sleep); err != nil {
			return
		}
	}
}

// restart replaces this process with the binary at its path (an update put a new one there, or a
// rollback put the previous one back). It returns only if that failed: the update is then undone.
func restart(agent *mamelet.Agent) {
	log.Printf("mamelet: restarting into %s", noderev.MameletPath)
	err := syscall.Exec(noderev.MameletPath, os.Args, os.Environ())
	log.Printf("mamelet: cannot restart: %v", err)
	if aerr := agent.AbortRestart(err); aerr != nil {
		log.Printf("mamelet: going back to the previous mamelet: %v", aerr)
	}
}

// rollBack puts the previous mamelet back and restarts into it.
func rollBack(agent *mamelet.Agent, why string) {
	log.Printf("mamelet: going back to the previous mamelet: %s", why)
	if err := agent.Rollback(why); err != nil {
		log.Printf("mamelet: cannot go back: %v", err)
		return
	}
	restart(agent)
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

// keepCertificate renews the client certificate once less than a third of its life is left, and says
// (once per change, not every round) when it cannot, or when it has ended. A Control Plane older
// than this mamelet does not know the request: the certificate is then used as it is.
func keepCertificate(ctx context.Context, cfg mamelet.Config, client *mamelet.Client, now time.Time, last *string) {
	end := client.CertNotAfter().Format("2006-01-02")
	msg := ""
	if client.CertExpired(now) {
		msg = fmt.Sprintf("MAM-PKI-0102: the client certificate ended %s, so the Control Plane refuses this node\n"+
			"  hint: ask the Control Plane's operator for a new one-time token (mamela controlplane reenroll --node %s), write it to %s, "+
			"delete %s/cert.pem and restart mamelet (docs/recovery.md)", end, cfg.Node, cfg.EnrollTokenFile, cfg.StateDir)
	} else if renewed, err := client.RenewIfDue(ctx, now); renewed {
		log.Printf("mamelet: renewed the client certificate (valid until %s)", client.CertNotAfter().Format("2006-01-02"))
	} else if errors.Is(err, mamelet.ErrRenewUnsupported) {
		msg = fmt.Sprintf("MAM-PKI-0104: the client certificate ends %s, and the Control Plane does not renew certificates (it is older than this mamelet)\n"+
			"  hint: update the Control Plane (mamela serve) before then; a certificate that ended needs the node enrolled again (docs/recovery.md)", end)
	} else if err != nil {
		msg = fmt.Sprintf("MAM-PKI-0104: could not renew the client certificate (it ends %s): %v\n  hint: it is tried again every hour; the Control Plane's log says why it refused", end, err)
	}
	if msg != "" && msg != *last {
		log.Printf("mamelet: %s", msg)
	}
	*last = msg
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
