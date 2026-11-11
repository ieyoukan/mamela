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
	if bin, err := os.ReadFile(site.ControlPlane.MameletBinary); err == nil {
		srv.MameletBinary = bin // for cloud machines' user-data seeds (ADR 0005)
	} else {
		logger.Printf("no mamelet binary to serve to cloud machines (%v)", err)
	}
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
