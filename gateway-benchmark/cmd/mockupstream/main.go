// Command mockupstream serves the deterministic model endpoint that every
// gateway under test proxies to.
//
//	go run ./benchmarks/gateway/cmd/mockupstream -addr :9800
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/upstream"
)

func main() {
	addr := flag.String("addr", ":9800", "listen address")
	flag.Parse()

	srv := upstream.New()

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// No write timeout: streaming responses and the deliberate hang fault
		// both outlive any sane one, and the harness controls its own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("mock upstream listening on %s", *addr)
	log.Printf("  models:  POST %s/v1/chat/completions  (and /p/{profile}/v1/...)", *addr)
	log.Printf("  control: POST %s/__control/profile/{name}", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
