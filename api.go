package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Web UI (embedded)
// ---------------------------------------------------------------------------

//go:embed index.html
var indexHTML string

// ---------------------------------------------------------------------------
// API server
// ---------------------------------------------------------------------------

type APIServer struct {
	engine *SyncEngine
	token  string
}

func (a *APIServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, indexHTML)
	})
	mux.HandleFunc("/api/status", a.auth(a.status))
	mux.HandleFunc("/api/mode", a.auth(a.setMode))
	mux.HandleFunc("/api/push", a.auth(a.push))
	mux.HandleFunc("/api/pull", a.auth(a.pull))
	return mux
}

func (a *APIServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			got := r.Header.Get("Authorization")
			if got != "Bearer "+a.token {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, "unauthorized")
				return
			}
		}
		next(w, r)
	}
}

type statusResponse struct {
	MPDState      string        `json:"mpd_state"`
	Mode          string        `json:"mode"`
	Current       *mpdEntry     `json:"current"`
	PositionMs    *int          `json:"position_ms"`
	Queue         []mpdEntry    `json:"queue"`
	RemoteChanged string        `json:"remote_changed"`
	LastPushTS    string        `json:"last_push_ts"`
	RemoteQueue   []remoteEntry `json:"remote_queue"`
	RemoteCurrent string        `json:"remote_current"`
	RemotePosMs   *int          `json:"remote_position_ms"`
}

type mpdEntry struct {
	File   string `json:"file"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Track  int    `json:"track"`
}

type remoteEntry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Track  int    `json:"track"`
}

func toMPDEntry(s map[string]any) mpdEntry {
	track := 0
	if t := intVal(s, "track"); t != nil {
		track = *t
	}
	return mpdEntry{
		File:   strVal(s, "file"),
		Title:  strVal(s, "title"),
		Artist: strVal(s, "artist"),
		Album:  strVal(s, "album"),
		Track:  track,
	}
}

func (a *APIServer) status(w http.ResponseWriter, r *http.Request) {
	st := statusResponse{
		MPDState:   a.engine.mpd.state(),
		PositionMs: a.engine.mpd.elapsedMs(),
		Mode:       a.engine.getMode(),
	}
	if cur := a.engine.mpd.currentsong(); cur["file"] != nil {
		e := toMPDEntry(cur)
		st.Current = &e
	}
	for _, s := range a.engine.mpd.playlistinfo() {
		st.Queue = append(st.Queue, toMPDEntry(s))
	}
	a.engine.mu.Lock()
	if a.engine.lastPushTS.IsZero() {
		st.LastPushTS = ""
	} else {
		st.LastPushTS = a.engine.lastPushTS.Format(time.RFC3339)
	}
	a.engine.mu.Unlock()
	if pq, err := a.engine.subsonic.GetPlayQueue(); err == nil && pq != nil {
		st.RemoteChanged = pq.Changed
		st.RemoteCurrent = pq.Current
		for _, e := range pq.Entry {
			st.RemoteQueue = append(st.RemoteQueue, remoteEntry{
				ID: e.ID, Title: e.Title, Artist: e.Artist, Album: e.Album, Track: e.Track,
			})
		}
		if ms := positionFor(pq, 0); ms != nil && st.RemoteCurrent != "" {
			st.RemotePosMs = ms
		}
	}
	writeJSON(w, st)
}

func (a *APIServer) push(w http.ResponseWriter, r *http.Request) {
	a.engine.pushQueue()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (a *APIServer) setMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := a.engine.setMode(body.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "mode": a.engine.getMode()})
}

func (a *APIServer) pull(w http.ResponseWriter, r *http.Request) {
	a.engine.pullQueue()
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
