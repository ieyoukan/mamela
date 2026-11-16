package main

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/store"
)

// agentCert is the certificate of one enrolled mamelet, as far as the database knows it. The
// database holds the serial only; when the certificate ends is noted in the event log when it is
// issued. A node enrolled before that was kept has none: its end is the enrollment and a year (what
// every certificate had before renewal), flagged as an estimate.
type agentCert struct {
	Agent     store.Agent
	NotAfter  time.Time
	Estimated bool
}

func listAgentCerts(ctx context.Context, st *store.Store) ([]agentCert, error) {
	agents, err := st.Agents(ctx)
	if err != nil {
		return nil, err
	}
	ends, err := st.AgentCertNotAfter(ctx)
	if err != nil {
		return nil, err
	}
	var out []agentCert
	for _, a := range agents {
		c := agentCert{Agent: a, NotAfter: ends[a.Node]}
		if c.NotAfter.IsZero() {
			c.NotAfter, c.Estimated = a.Enrolled.AddDate(1, 0, 0), true
		}
		out = append(out, c)
	}
	return out, nil
}

// status is "ok", "ends soon" (within cp.CertWarnWithin) or "ended".
func (c agentCert) status(now time.Time) string {
	switch left := c.NotAfter.Sub(now); {
	case left <= 0:
		return "ended"
	case left < cp.CertWarnWithin:
		return "ends soon"
	}
	return "ok"
}

// problem is the warning for a certificate that has ended or ends soon ("" when there is nothing to say).
func (c agentCert) problem(now time.Time) string {
	est := ""
	if c.Estimated {
		est = " (counted from its enrollment: the end was not recorded)"
	}
	switch left := c.NotAfter.Sub(now); {
	case left <= 0:
		return fmt.Sprintf("MAM-PKI-0102: node %s: the mamelet's certificate ended %s%s, so the Control Plane refuses it\n"+
			"  hint: enroll it again: mamela controlplane reenroll --node %s, and follow what it prints (docs/recovery.md)",
			c.Agent.Node, c.NotAfter.Format("2006-01-02"), est, c.Agent.Node)
	case left < cp.CertWarnWithin:
		return fmt.Sprintf("MAM-PKI-0101: node %s: the mamelet's certificate ends %s, in %d days%s, and has not been renewed\n"+
			"  hint: a mamelet from 0.3.2 on renews it by itself from a Control Plane that supports it: update this node's mamelet (docs/mamelet-update.md); "+
			"before it ends, or after, mamela controlplane reenroll --node %s enrolls it again (docs/recovery.md)",
			c.Agent.Node, c.NotAfter.Format("2006-01-02"), int(left.Hours()/24), est, c.Agent.Node)
	}
	return ""
}

// agentCertWarnings are the warnings about the mamelets whose certificate ended or ends soon.
func agentCertWarnings(ctx context.Context, st *store.Store, now time.Time) ([]string, error) {
	certs, err := listAgentCerts(ctx, st)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range certs {
		if w := c.problem(now); w != "" {
			out = append(out, w)
		}
	}
	return out, nil
}

// caWarnWithin is how long before the CA ends it is warned about. Every mamelet is pinned to the CA,
// and there is no way to replace it without enrolling every node again (docs/recovery.md).
const caWarnWithin = 365 * 24 * time.Hour

// caWarning is the warning about a CA that ends within a year ("" when there is nothing to say).
func caWarning(ca *x509.Certificate, now time.Time) string {
	left := ca.NotAfter.Sub(now)
	if left >= caWarnWithin {
		return ""
	}
	return fmt.Sprintf("MAM-PKI-0106: the Control Plane's CA ends %s (%s). Every mamelet trusts this CA, and no certificate it signs outlives it\n"+
		"  hint: replacing it means enrolling every node again, and there is no overlap in which both work: plan it ahead (docs/recovery.md, \"The CA and the revision key\")",
		ca.NotAfter.Format("2006-01-02"), map[bool]string{true: "already ended", false: fmt.Sprintf("in %d days", int(left.Hours()/24))}[left <= 0])
}

// watchAgentCerts logs the same warnings when the Control Plane starts and once a day after: an
// ended certificate never reaches a handler, and a mamelet that is not asking says nothing. It
// watches the CA, too.
func watchAgentCerts(ctx context.Context, st *store.Store, ca *x509.Certificate, logger *log.Logger) {
	for {
		if w := caWarning(ca, wallClock()); w != "" {
			logger.Print(w)
		}
		if ws, err := agentCertWarnings(ctx, st, wallClock()); err != nil {
			logger.Printf("could not check the mamelets' certificates: %v", err)
		} else {
			for _, w := range ws {
				logger.Print(w)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// runAgents lists the enrolled mamelets with when their certificates end.
func runAgents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela controlplane agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: mamela controlplane agents [--db <state.db>]")
		return 2
	}
	if _, err := os.Stat(*db); err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	certs, err := listAgentCerts(ctx, st)
	if err != nil {
		return report(stderr, err)
	}
	now := wallClock()
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tVMID\tENROLLED\tCERTIFICATE ENDS\tSTATUS")
	for _, c := range certs {
		ends := c.NotAfter.Format("2006-01-02")
		if c.Estimated {
			ends += " (estimated)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", c.Agent.Node, c.Agent.VMID, c.Agent.Enrolled.Format("2006-01-02"), ends, c.status(now))
	}
	tw.Flush()
	for _, c := range certs {
		if w := c.problem(now); w != "" {
			fmt.Fprintln(stdout, "\n"+w)
		}
	}
	return 0
}

// maxReenrollTTL is the longest a token for enrolling again may stay good.
const maxReenrollTTL = 24 * time.Hour

// runReenroll issues a one-time token for a node that is registered already, for when its
// certificate ended (or was lost): the node enrolls again with it, which replaces its registration.
func runReenroll(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela controlplane reenroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	node := fs.String("node", "", "the node to enroll again (required)")
	vmid := fs.Int("vmid", 0, "the node's VMID (its config.json), only for a node this database does not know (a lost database)")
	ttl := fs.Duration("ttl", time.Hour, "how long the token stays good (at most 24h)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela controlplane reenroll --node <name> [--db <state.db>] [--ttl 1h]\n\n"+
			"Prints a one-time token with which the node's mamelet can enroll again, and what to do on the node.\n"+
			"For a mamelet whose certificate ended (it refuses to connect), or whose key was lost. Enrolling again\n"+
			"replaces the node's registration: its old certificates stop working, and plans made before are made again.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *node == "" {
		fs.Usage()
		return 2
	}
	if *ttl <= 0 || *ttl > maxReenrollTTL {
		return report(stderr, fmt.Errorf("--ttl must be more than 0 and at most %s: the token is a password for the node's identity", maxReenrollTTL))
	}
	if _, err := os.Stat(*db); err != nil {
		return report(stderr, err)
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	a, ok, err := st.Agent(ctx, *node)
	if err != nil {
		return report(stderr, err)
	}
	switch {
	case !ok && *vmid <= 0:
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0301: node %s is not registered in %s, so there is nothing to enroll again\n"+
			"  hint: a machine that Mamela creates gets its first token in its seed (mamela apply); `mamela controlplane agents` lists the registered nodes. "+
			"If its record was lost with a database, give --vmid <the vmid in the node's /etc/mamelet/config.json>", *node, *db)})
	case ok && *vmid > 0 && *vmid != a.VMID:
		return report(stderr, told{fmt.Sprintf("MAM-PKI-0301: node %s is registered with VMID %d, not %d; a token for another machine is not issued", *node, a.VMID, *vmid)})
	case !ok:
		a = store.Agent{Node: *node, VMID: *vmid}
	}
	token, err := st.IssueEnrollToken(ctx, a.Node, a.VMID, "", *ttl)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "A one-time token for node %s (VMID %d), good for %s:\n\n  %s\n\n", a.Node, a.VMID, *ttl, token)
	if !ok {
		fmt.Fprintf(stdout, "(this database has no record of the node: it is registered when it enrolls)\n\n")
	}
	fmt.Fprintf(stdout, "On the node, as root (the paths are those in /etc/mamelet/config.json: enrollTokenFile, stateDir):\n"+
		"  1. systemctl stop mamelet\n"+
		"  2. write the token and a newline to the file named by enrollTokenFile (mode 0600; /etc/mamelet/enroll-token for a node Mamela made)\n"+
		"  3. rm <stateDir>/cert.pem   (key.pem is replaced when it enrolls)\n"+
		"  4. systemctl start mamelet   (it enrolls, deletes the token file, and carries on)\n\n"+
		"The token works once. Enrolling replaces the node's registration, so the certificates it had stop working;\n"+
		"approved plans for this node made before must be made again (MAM-NODE-0101). docs/recovery.md has the details.\n")
	return 0
}
