// JobHub is a private job manager and timeline, not an outbound agent runtime.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/demo"
	"github.com/sergey-yakushevich/jobhub/internal/store"
	"github.com/sergey-yakushevich/jobhub/internal/workspace"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	key := os.Getenv("JOBHUB_API_KEY")
	if len(key) < 32 {
		log.Fatal("JOBHUB_API_KEY is required (at least 32 characters); use scripts/dev.sh for local setup")
	}
	host, port := env("HOST", "127.0.0.1"), env("PORT", "8096")
	origin := env("BASE_URL", "http://"+host+":"+port)
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		log.Fatal("BASE_URL must be an http(s) origin without a path")
	}
	if u.Scheme != "https" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		log.Fatal("non-loopback binding requires an HTTPS BASE_URL")
	}
	st, err := store.Open(env("DB_PATH", "jobs.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()
	isDemo := strings.EqualFold(os.Getenv("JOBHUB_DEMO"), "true") || os.Getenv("JOBHUB_DEMO") == "1"
	if isDemo {
		if err := demo.Seed(st, time.Now()); err != nil {
			log.Fatalf("seed mock data: %v", err)
		}
	}
	server := &http.Server{Addr: host + ":" + port, Handler: workspace.New(st, key, origin, isDemo), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("JobHub ready at %s (mock data: %t)", origin, isDemo)
	log.Fatal(server.ListenAndServe())
}
