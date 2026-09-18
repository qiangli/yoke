// Command serve runs the meet web room for the browser e2e suite.
//
// It is deliberately tiny and lives beside the tests rather than in cmd/: it is
// not a product surface, it is the harness's server. Build it with
// -tags meetspa (after `npm run build`) so the SPA under test is the one in
// this checkout.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/qiangli/yoke/pkg/meet"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	flag.Parse()

	srv := &http.Server{Addr: *addr, Handler: meet.Handler(context.Background())}
	log.Printf("meet e2e server listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
