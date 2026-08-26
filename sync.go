package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Sync engine
// ---------------------------------------------------------------------------

type SyncEngine struct {
	subsonic *Subsonic
	mpd      *MPD
	cfg      Config

	fpToID   map[string]string
	idToFile map[string]string

	lastPushSig   string
	lastRemoteSig string
	lastPushTS    time.Time

	mode      atomic.Value // string: auto|manual|push|pull
	stateFile string

	mu sync.Mutex
}

type fingerprint string

func makeFP(title, artist, album string, track, disc *int) fingerprint {
	t, d := 0, 0
	if track != nil {
		t = *track
	}
	if disc != nil {
		d = *disc
	}
	return fingerprint(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", title, artist, album, t, d))
}

func NewSyncEngine(sub *Subsonic, m *MPD, cfg Config) *SyncEngine {
	e := &SyncEngine{
		subsonic:  sub,
		mpd:       m,
		cfg:       cfg,
		stateFile: cfg.StateFile,
		fpToID:    map[string]string{},
		idToFile:  map[string]string{},
	}
	e.mode.Store("manual")
	if e.stateFile != "" {
		if data, err := os.ReadFile(e.stateFile); err == nil {
			if m := strings.TrimSpace(string(data)); validMode(m) {
				e.mode.Store(m)
				log.Printf("mode: restored %q from %s", m, e.stateFile)
			}
		}
	}
	return e
}

func validMode(m string) bool {
	switch m {
	case "auto", "manual", "push", "pull":
		return true
	}
	return false
}

// getMode returns the current sync mode. Safe to call from any goroutine.
func (e *SyncEngine) getMode() string { return e.mode.Load().(string) }

// setMode validates and stores a new sync mode, persisting it best-effort.
func (e *SyncEngine) setMode(m string) error {
	m = strings.TrimSpace(m)
	if !validMode(m) {
		return fmt.Errorf("invalid mode %q (want auto|manual|push|pull)", m)
	}
	e.mode.Store(m)
	if e.stateFile != "" {
		if err := os.MkdirAll(filepath.Dir(e.stateFile), 0o755); err != nil {
			log.Printf("mode: cannot create state dir for %s: %v", e.stateFile, err)
		} else if err := os.WriteFile(e.stateFile, []byte(m+"\n"), 0o644); err != nil {
			log.Printf("mode: cannot persist %q to %s: %v", m, e.stateFile, err)
		}
	}
	return nil
}

// dbg logs at INFO when verbose is on, and stays silent otherwise.
func (e *SyncEngine) dbg(format string, args ...any) {
	if e.cfg.Verbose {
		log.Printf(format, args...)
	}
}

func (e *SyncEngine) resolvePush(song map[string]any) string {
	fp := string(makeFP(
		strVal(song, "title"),
		strVal(song, "artist"),
		strVal(song, "album"),
		intVal(song, "track"),
		intVal(song, "disc"),
	))
	if id, ok := e.fpToID[fp]; ok {
		return id
	}
	title := strVal(song, "title")
	artist := strVal(song, "artist")
	album := strVal(song, "album")
	track := intVal(song, "track")
	disc := intVal(song, "disc")
	id, err := e.subsonic.ResolveID(title, artist, album, track, disc)
	if err == nil && id != "" {
		e.fpToID[fp] = id
	}
	return id
}

func strVal(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func intVal(m map[string]any, k string) *int {
	if v, ok := m[k].(string); ok {
		if i, err := strconv.Atoi(v); err == nil {
			return &i
		}
	}
	if f, ok := m[k].(float64); ok {
		i := int(f)
		return &i
	}
	return nil
}

func (e *SyncEngine) mpdSignature() (string, bool) {
	songs := e.mpd.playlistinfo()
	if len(songs) == 0 {
		return "", false
	}
	curFile := e.mpd.currentsong()["file"]
	var ids []string
	var curID string
	for _, s := range songs {
		id := e.resolvePush(s)
		if id != "" {
			ids = append(ids, id)
			if s["file"] == curFile {
				curID = id
			}
		}
	}
	if len(ids) == 0 {
		return "", false
	}
	return fmt.Sprintf("%v\x00%s", ids, curID), true
}

func (e *SyncEngine) seedBaseline() {
	sig, ok := e.mpdSignature()
	if ok {
		e.mu.Lock()
		e.lastPushSig = sig
		e.mu.Unlock()
		log.Println("Seeded baseline from MPD (no push)")
	}
}

func (e *SyncEngine) pushQueue() {
	sig, ok := e.mpdSignature()
	if !ok {
		return
	}
	e.mu.Lock()
	last := e.lastPushSig
	e.mu.Unlock()
	if sig == last {
		return
	}
	ids, curID := parseSig(sig)
	position := e.mpd.elapsedMs()
	if err := e.subsonic.SavePlayQueue(ids, curID, position); err != nil {
		log.Printf("Push failed: %v", err)
		return
	}
	e.mu.Lock()
	e.lastPushSig = sig
	e.lastPushTS = time.Now()
	e.mu.Unlock()
	log.Printf("Pushed %d songs to Navidrome (current=%s)", len(ids), curID)
}

func parseSig(sig string) ([]string, string) {
	parts := strings.SplitN(sig, "\x00", 2)
	if len(parts) != 2 {
		return nil, ""
	}
	var ids []string
	trimmed := strings.Trim(parts[0], "[]")
	if trimmed != "" {
		for _, p := range strings.Split(trimmed, " ") {
			p = strings.Trim(p, "\"")
			if p != "" {
				ids = append(ids, p)
			}
		}
	}
	return ids, parts[1]
}

func (e *SyncEngine) pullQueue() {
	state := e.mpd.state()
	if state == "play" {
		return // never stomp active playback
	}
	pq, err := e.subsonic.GetPlayQueue()
	if err != nil || pq == nil {
		return
	}
	if len(pq.Entry) == 0 {
		return
	}
	var remoteFiles []string
	var remoteIDs []string
	for _, entry := range pq.Entry {
		f := e.resolvePull(entry)
		if f != "" {
			remoteFiles = append(remoteFiles, f)
			remoteIDs = append(remoteIDs, entry.ID)
		}
	}
	if len(remoteFiles) == 0 {
		return
	}
	// staleness guard: only pull if remote changed after our last push
	remoteChanged := parseChanged(pq.Changed)
	e.mu.Lock()
	lpt := e.lastPushTS
	e.mu.Unlock()
	if remoteChanged != nil && remoteChanged.Before(lpt) {
		e.dbg("Remote queue is stale (changed=%s < last push=%s); skipping pull", pq.Changed, lpt.Format(time.RFC3339))
		return
	}
	sig := fmt.Sprintf("%v\x00%s", remoteIDs, pq.Current)
	e.mu.Lock()
	lrs := e.lastRemoteSig
	e.mu.Unlock()
	if sig == lrs {
		return
	}
	localFiles := e.mpd.queueFiles()
	curID := pq.Current
	curIndex := -1
	if curID != "" {
		for i, id := range remoteIDs {
			if id == curID {
				curIndex = i
				break
			}
		}
	}
	var curIndexPtr *int
	if curIndex >= 0 {
		curIndexPtr = &curIndex
	}
	if sameQueue(localFiles, remoteFiles) {
		localCur := strVal(e.mpd.currentsong(), "file")
		if curIndexPtr != nil {
			want := remoteFiles[*curIndexPtr]
			if localCur != want {
				log.Printf("Remote current changed; moving cursor to %s", want)
				position := positionFor(pq, *curIndexPtr)
				e.mpd.setCurrentIndex(*curIndexPtr, state == "pause", position)
			}
		}
		e.mu.Lock()
		e.lastRemoteSig = sig
		e.mu.Unlock()
		return
	}
	log.Printf("Remote queue differs; loading %d songs into MPD", len(remoteFiles))
	position := positionFor(pq, curIndex)
	e.mpd.loadFiles(remoteFiles, curIndexPtr, state == "pause", position)
	e.mu.Lock()
	e.lastRemoteSig = sig
	e.mu.Unlock()
}

func positionFor(pq *PlayQueue, index int) *int {
	if index < 0 {
		return nil
	}
	ms := int(pq.PositionMillis)
	if ms == 0 && pq.Position > 0 {
		ms = int(pq.Position * 1000)
	}
	if ms > 0 {
		return &ms
	}
	return nil
}

func (e *SyncEngine) resolvePull(entry PlayQueueEntry) string {
	if id, ok := e.idToFile[entry.ID]; ok {
		return id
	}
	if entry.Title == "" {
		return ""
	}
	f := e.mpd.findFile(entry.Title, entry.Artist, entry.Album, intPtr(entry.Track), intPtr(entry.DiscNumber))
	if f != "" {
		e.idToFile[entry.ID] = f
	}
	return f
}

func intPtr(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func parseChanged(value string) *time.Time {
	if value == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.0000000",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return &t
		}
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		t := time.Unix(int64(f), 0)
		return &t
	}
	return nil
}

func sameQueue(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
