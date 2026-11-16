package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	sitedef "github.com/ieyoukan/mamela/internal/site"
)

// runServe serves mamelet: enrollment, revisions and reports.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela serve [flags]\n\n"+
			"Serves mamelet (the agent on the machines) on controlplane.listen: one-time enrollment, signed revisions,\n"+
			"reports. It changes nothing by itself; mamelet acts only on what an approved apply granted, and on\n"+
			"the repairs the policy allows.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	site, err := sitedef.Load(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	if !site.ControlPlane.Enabled() {
		return report(stderr, errors.New("the site has no [controlplane] section (see mamela.site.example.toml)"))
	}
	// What was there before this start, to tell a first start from the loss of one of the two: the
	// keys are made by the first start only, and the database is what knows which mamelets trust them.
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	keysBefore := len(cp.MissingKeys(dir)) == 0
	_, statErr := os.Stat(*db)
	dbBefore := statErr == nil
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	pki, err := loadPKI(ctx, site, st)
	if err != nil {
		return report(stderr, err)
	}
	logger := log.New(stderr, "mamela serve: ", log.LstdFlags)
	if keysBefore && !dbBefore {
		logger.Printf("MAM-PKI-0203: %s did not exist, but the keys in %s did: this database is new, so no mamelet is registered in it\n"+
			"  hint: if a database was lost, restore it from a backup (mamela restore --in <backup>) and start again; the nodes cannot connect until then (docs/recovery.md)", *db, dir)
	}
	srv := &cp.Server{Store: st, PKI: pki, Now: wallClock, RevisionTTL: 24 * time.Hour, Log: logger}
	if bin, err := os.ReadFile(site.ControlPlane.MameletBinary); err == nil {
		srv.MameletBinary = bin // for cloud machines' user-data seeds (ADR 0005)
	} else {
		logger.Printf("no mamelet binary to serve to cloud machines (%v)", err)
	}
	// ReadTimeout bounds the whole request (and the TLS handshake before it); no handler needs more
	// than a megabyte from a client. There is no WriteTimeout: the binary is large, and a wait lasts
	// half a minute by design.
	hs := &http.Server{Addr: site.ControlPlane.Listen, Handler: srv.Handler(), TLSConfig: srv.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: srv.HTTPErrorLog()}
	go watchAgentCerts(ctx, st, pki.CA, logger)
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
