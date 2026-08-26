package main

// baton — bridge MPD's play queue to Navidrome's Subsonic play queue
// (savePlayQueue / getPlayQueue) so you can continue listening on any device,
// Spotify-style handoff across MPD and Subsonic clients.
//
// Zero-dependency single binary: the web UI is embedded with //go:embed and the
// MPD client speaks the raw MPD TCP protocol. See README.md. Source is split by
// concern into config.go, navidrome.go, mpd.go, sync.go and api.go.

import (
	"context"
	"log"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if cfg.NavidromeURL == "" || cfg.NavidromeUser == "" || cfg.NavidromePass == "" {
		log.Fatal("NAVIDROME_URL, NAVIDROME_USER and NAVIDROME_PASSWORD are required. Set them " +
			"in your --env-file, as environment variables, or with --navidrome-url/--navidrome-user/" +
			"--navidrome-password.")
	}
	// Accept "host:port" and add a scheme.
	cfg.NavidromeURL = normalizeURL(cfg.NavidromeURL)
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}

	sub := NewSubsonic(cfg.NavidromeURL, cfg.NavidromeUser, cfg.NavidromePass, cfg.NavidromeLegacy)
	mpd := NewMPD(cfg.MPDHost, cfg.MPDPort, cfg.MPDPassword)
	if err := mpd.ensureConnected(); err != nil {
		log.Fatalf("Cannot connect to MPD: %v", err)
	}

	engine := NewSyncEngine(sub, mpd, cfg)
	engine.seedBaseline()

	// API + web UI
	api := &APIServer{engine: engine, token: cfg.APIToken}
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: api.handler(),
	}
	go func() {
		log.Printf("Web UI + API listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("API server error: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("Starting baton (mode=%s, poll=%s)", engine.getMode(), cfg.PollInterval)
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := mpd.ensureConnected(); err != nil {
				log.Printf("MPD reconnect failed: %v", err)
				continue
			}
			if !sub.Ping() {
				log.Printf("Navidrome unreachable; retrying")
			}
			switch engine.getMode() {
			case "auto", "push":
				engine.pushQueue()
			}
			if m := engine.getMode(); m == "auto" || m == "pull" {
				engine.pullQueue()
			}
		}
	}
}
