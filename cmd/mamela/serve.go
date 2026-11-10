package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/lab"
)

// runServe serves mamelet: enrollment, revisions and reports (docs/lab.md).
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela serve [flags]\n\n"+
			"Serves mamelet (the agent in lab VMs) on controlplane.listen: one-time enrollment, signed revisions,\n"+
			"reports. It changes nothing by itself; mamelet acts only on what an approved apply granted, and on\n"+
			"the repairs the policy allows.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	if !site.ControlPlane.Enabled() {
		return report(stderr, errors.New("the site has no [controlplane] section (see lab/site.example.toml)"))
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	pki, err := loadPKI(site)
	if err != nil {
		return report(stderr, err)
	}
	logger := log.New(stderr, "mamela serve: ", log.LstdFlags)
	srv := &cp.Server{Store: st, PKI: pki, Now: wallClock, RevisionTTL: 24 * time.Hour, Log: logger}
	hs := &http.Server{Addr: site.ControlPlane.Listen, Handler: srv.Handler(), TLSConfig: srv.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	fmt.Fprintf(stdout, "serving mamelet on https://%s (mamelet uses %s); Ctrl-C to stop\n", site.ControlPlane.Listen, site.ControlPlane.URL)
	if err := hs.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return report(stderr, err)
	}
	return 0
}

// runTunnel opens an SSH reverse tunnel into a lab VM so its mamelet reaches `mamela serve` here.
func runTunnel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela lab tunnel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	host := fs.String("host", "", "the VM's address (default: as reported by qemu-guest-agent)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela lab tunnel [flags] <vm-name>\n\n"+
			"Runs ssh -R so that controlplane.url inside the VM reaches controlplane.listen on this machine.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	if !site.ControlPlane.Enabled() {
		return report(stderr, errors.New("the site has no [controlplane] section"))
	}
	addr := *host
	if addr == "" {
		st, err := openStore(ctx, *db)
		if err != nil {
			return report(stderr, err)
		}
		vmid, ok, err := st.VMID(ctx, name)
		st.Close()
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			return report(stderr, fmt.Errorf("Mamela has no VM %s", name))
		}
		operating, err := labClient(site, site.Proxmox.TokenID, site.Proxmox.TokenSecretFile)
		if err != nil {
			return report(stderr, err)
		}
		ips, err := operating.GuestIPv4(ctx, site.Proxmox.Node, vmid)
		if err != nil {
			return report(stderr, fmt.Errorf("ask qemu-guest-agent in VM %d for its address: %w (or pass --host)", vmid, err))
		}
		if len(ips) == 0 {
			return report(stderr, fmt.Errorf("VM %d reports no IPv4 address yet", vmid))
		}
		addr = ips[0]
	}
	remotePort, err := portOf(site.ControlPlane.URL)
	if err != nil {
		return report(stderr, err)
	}
	sshArgs := []string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-o", "StrictHostKeyChecking=accept-new",
		"-R", "127.0.0.1:" + remotePort + ":" + site.ControlPlane.Listen}
	if j := site.ControlPlane.TunnelJump; j != "" {
		sshArgs = append(sshArgs, "-J", j)
	}
	sshArgs = append(sshArgs, site.Guest.User+"@"+addr)
	fmt.Fprintf(stdout, "tunnel to %s (%s): VM 127.0.0.1:%s -> %s; Ctrl-C to close\n", name, addr, remotePort, site.ControlPlane.Listen)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return report(stderr, fmt.Errorf("ssh: %w", err))
	}
	return 0
}

func portOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("controlplane.url %q needs an explicit port: %w", raw, err)
	}
	return port, nil
}
