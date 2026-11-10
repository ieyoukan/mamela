// Command kv-api serves the example key-value API (in memory) on the loopback interface, for
// trying the example provider with mamela plan and apply (examples/kv-demo).
//
//	go run ./examples/kv/cmd/kv-api -addr 127.0.0.1:8099 -token-file /path/to/token
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "address to listen on (loopback only)")
	tokenFile := flag.String("token-file", "", "require \"Authorization: Bearer <token>\" with the token in this file")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalf("%s: listen on a loopback address only", *addr)
	}
	var h http.Handler = kvapi.New()
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatal(err)
		}
		h = kvapi.RequireToken(strings.TrimSpace(string(b)), h)
	}
	log.Printf("kv-api on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}
