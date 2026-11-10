package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

const labUsage = `Usage:
  mamela lab bootstrap  --site <file>                    print the one-time root setup script (does not run it)
  mamela lab preflight  --site <file> [--baseline <out>]  read-only checks before a live test; records a baseline
  mamela lab postflight --site <file> --baseline <in>     read-only check that nothing outside the lab changed
  mamela lab tunnel     --site <file> <vm-name>          SSH reverse tunnel so the VM's mamelet reaches mamela serve

See docs/lab.md.
`

func runLab(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, labUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub == "tunnel" {
		return runTunnel(ctx, rest, stdout, stderr)
	}
	fs := flag.NewFlagSet("mamela lab "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	baseline := fs.String("baseline", ".mamela/lab-baseline.json", "baseline of guests outside the lab")
	fs.Usage = func() { fmt.Fprint(stderr, labUsage) }
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}

	switch sub {
	case "bootstrap":
		fmt.Fprint(stdout, site.BootstrapScript())
		return 0
	case "preflight", "postflight":
	default:
		fmt.Fprintf(stderr, "mamela lab: unknown command %q\n\n%s", sub, labUsage)
		return 2
	}

	audit, err := labClient(site, site.Proxmox.AuditTokenID, site.Proxmox.AuditTokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
	if err != nil {
		return report(stderr, err)
	}
	facts, err := lab.Gather(ctx, site, audit, operating)
	if err != nil {
		return report(stderr, err)
	}

	var findings []lab.Finding
	if sub == "preflight" {
		findings = lab.Check(site, facts)
		if !lab.Blocked(findings) {
			if err := os.MkdirAll(filepath.Dir(*baseline), 0o700); err != nil {
				return report(stderr, err)
			}
			if err := os.WriteFile(*baseline, lab.NewBaseline(site, facts).Encode(), 0o600); err != nil {
				return report(stderr, err)
			}
		}
	} else {
		data, err := os.ReadFile(*baseline)
		if err != nil {
			return report(stderr, fmt.Errorf("read the baseline from preflight: %w", err))
		}
		before, err := lab.DecodeBaseline(data)
		if err != nil {
			return report(stderr, err)
		}
		findings = lab.Compare(site, before, facts)
	}

	for _, f := range findings {
		fmt.Fprintf(stdout, "%-5s %s  %s\n", f.Severity, f.Code, f.Message)
	}
	if lab.Blocked(findings) {
		fmt.Fprintf(stdout, "\n%s: BLOCKED. Do not run live tests until every BLOCK is resolved.\n", sub)
		return 1
	}
	if sub == "preflight" {
		fmt.Fprintf(stdout, "\npreflight passed; baseline of %d guests outside the lab written to %s\n",
			len(lab.NewBaseline(site, facts).Guests), *baseline)
	} else {
		fmt.Fprintln(stdout, "\npostflight passed: nothing outside the lab changed")
	}
	return 0
}

func labClient(site *lab.Site, tokenID, secretFile string) (*proxmox.Client, error) {
	secret, err := lab.ReadSecret(secretFile)
	if err != nil {
		return nil, fmt.Errorf("token %s: %w", tokenID, err)
	}
	return proxmox.New(site.Proxmox.APIURL, tokenID, secret,
		proxmox.Options{Fingerprint: site.Proxmox.TLSFingerprint, CAFile: site.Proxmox.TLSCAFile})
}
