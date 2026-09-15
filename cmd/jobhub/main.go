// jobhub — the self-hosted job-leads board.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sergey-yakushevich/jobhub/internal/httpapi"
	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	dbPath := env("DB_PATH", "jobs.db")
	apiToken := os.Getenv("API_TOKEN")
	viewKey := os.Getenv("VIEW_KEY")
	if apiToken == "" || viewKey == "" {
		log.Fatal("API_TOKEN and VIEW_KEY are required")
	}
	baseURL := env("BASE_URL", "http://localhost:8080")
	addr := ":" + env("PORT", "8080")

	// Profile config comes before the store opens: the backfill migration
	// stamps DefaultJobProfile into un-profiled rows.
	if v := strings.TrimSpace(os.Getenv("DEFAULT_PROFILE")); v != "" {
		store.DefaultJobProfile = store.NormalizeProfile(v)
	}
	if v := os.Getenv("PROFILES"); v != "" {
		httpapi.SetKnownProfiles(strings.Split(v, ","))
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()

	server := httpapi.New(st, apiToken, viewKey)
	// Voice notes are optional: with a key the composer's recordings are
	// transcribed into the review note, without one they stay a preview.
	if key := os.Getenv("ELEVENLABS_API_KEY"); key != "" {
		stt := httpapi.NewElevenLabsSTT(key, os.Getenv("ELEVENLABS_STT_MODEL"))
		if base := os.Getenv("ELEVENLABS_BASE_URL"); base != "" {
			stt.BaseURL = strings.TrimRight(base, "/")
		}
		server.STT = stt
	}
	// The public URL may mount the app under a path (e.g. /trk); links in the
	// HTML pages must carry that prefix since the proxy strips it before us.
	// The origin feeds any absolute link the app mints.
	if u, err := url.Parse(baseURL); err == nil {
		server.BasePath = strings.TrimRight(u.Path, "/")
		server.Origin = u.Scheme + "://" + u.Host
	}
	log.Printf("jobhub listening on %s (db %s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, server))
}
