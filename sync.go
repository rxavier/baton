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
	pushed        bool // true once a real SavePlayQueue succeeded (baseline doesn't count)

	mode      atomic.Value // string: auto|manual|push|pull
	stateFile string

	// serializes push/pull so the poll loop and API handlers can't race
	syncMu sync.Mutex

	// remote queue cache so UI clients don't hammer Navidrome on every poll
	remoteMu  sync.Mutex
	remotePq  *PlayQueue
	remoteTS  time.Time
	remoteTTL time.Duration

	mu sync.Mutex
}

const defaultRemoteTTL = 10 * time.Second

type fingerprint string

func makeFP(title, artist, album string, track, disc, duration *int) fingerprint {
	t, d, dur := 0, 0, 0
	if track != nil {
		t = *track
	}
	if disc != nil {
		d = *disc
	}
	if duration != nil {
		dur = *duration
	}
	return fingerprint(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d", title, artist, album, t, d, dur))
}

func NewSyncEngine(sub *Subsonic, m *MPD, cfg Config) *SyncEngine {
	e := &SyncEngine{
		subsonic:  sub,
		mpd:       m,
		cfg:       cfg,
		stateFile: cfg.StateFile,
		remoteTTL: defaultRemoteTTL,
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
			return nil
		}
		// write-then-rename so a crash never leaves a corrupt state file
		tmp := e.stateFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(m+"\n"), 0o644); err != nil {
			log.Printf("mode: cannot persist %q to %s: %v", m, e.stateFile, err)
		} else if err := os.Rename(tmp, e.stateFile); err != nil {
			log.Printf("mode: cannot persist %q to %s: %v", m, e.stateFile, err)
			os.Remove(tmp)
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
		intVal(song, "time"),
	))
	if id, ok := e.fpToID[fp]; ok {
		return id
	}
	title := strVal(song, "title")
	artist := strVal(song, "artist")
	album := strVal(song, "album")
	track := intVal(song, "track")
	disc := intVal(song, "disc")
	duration := intVal(song, "time")
	id, err := e.subsonic.ResolveID(title, artist, album, track, disc, duration)
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
	e.pushed = false
	e.mu.Unlock()
	log.Println("Seeded baseline from MPD (no push)")
	}
}

// getRemoteQueue returns the last-known Navidrome queue, refreshing it
// (single-flight under remoteMu) when the cache is older than remoteTTL. UI
// clients poll every few seconds, so this caps getPlayQueue traffic instead
// of hitting Navidrome on every status request.
func (e *SyncEngine) getRemoteQueue() (*PlayQueue, error) {
	e.remoteMu.Lock()
	defer e.remoteMu.Unlock()
	if e.remotePq != nil && time.Since(e.remoteTS) < e.remoteTTL {
		return e.remotePq, nil
	}
	pq, err := e.subsonic.GetPlayQueue()
	if err != nil {
		return e.remotePq, err
	}
	e.remotePq = pq
	e.remoteTS = time.Now()
	return pq, nil
}

// setRemoteQueue caches a queue fetched by the sync loop so the UI can serve
// it without an extra Subsonic call.
func (e *SyncEngine) setRemoteQueue(pq *PlayQueue) {
	if pq == nil {
		return
	}
	e.remoteMu.Lock()
	e.remotePq = pq
	e.remoteTS = time.Now()
	e.remoteMu.Unlock()
}

func (e *SyncEngine) pushQueue() { e.pushQueueManual(false) }

// pushQueueManual performs a push. manual=true turns every run into a
// converging save: an unchanged-queue skip no longer suppresses it, so an
// explicit push (API button) always restores MPD's queue to Navidrome.
func (e *SyncEngine) pushQueueManual(manual bool) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	sig, ok := e.mpdSignature()
	if !ok {
		return
	}
	e.mu.Lock()
	last := e.lastPushSig
	pushed := e.pushed
	e.mu.Unlock()
	if sig == last && !pushed && !manual {
		return // seeded baseline, nothing saved yet, loop must not auto-push
	}
	if sig == last && pushed {
		// MPD unchanged since our last saved push, but the remote queue may
		// have been overwritten by another client (e.g. a phone player saving
		// its stale cache). Re-push unless the remote still matches.
		e.repushIfRemoteChanged(sig)
		return
	}
	// otherwise: either sig changed, or manual push after baseline (never
	// actually saved yet) — fall through to a plain save.
	ids, curID := parseSig(sig)
	position := e.mpd.elapsedMs()
	if err := e.subsonic.SavePlayQueue(ids, curID, position); err != nil {
		log.Printf("Push failed: %v", err)
		return
	}
	e.mu.Lock()
	e.lastPushSig = sig
	e.lastPushTS = time.Now()
	e.pushed = true
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
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	state := e.mpd.state()
	if state == "play" {
		return // never stomp active playback
	}
	pq, err := e.subsonic.GetPlayQueue()
	if err != nil {
		return
	}
	e.setRemoteQueue(pq)
	if pq == nil || len(pq.Entry) == 0 {
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

// repushIfRemoteChanged re-pushes the MPD queue when the remote queue was
// overwritten by another client. Returns true if a push was issued.
func (e *SyncEngine) repushIfRemoteChanged(sig string) bool {
	lastIDs, lastCur := parseSig(sig)
	pq, err := e.subsonic.GetPlayQueue()
	if err != nil {
		log.Printf("Push skipped (remote check failed): %v", err)
		return false
	}
	if pq == nil {
		pq = &PlayQueue{} // empty remote queue: definitely differs
	}
	remoteIDs := make([]string, 0, len(pq.Entry))
	for _, en := range pq.Entry {
		remoteIDs = append(remoteIDs, en.ID)
	}
	if sameStringSet(remoteIDs, lastIDs) && e.matchesRemoteCurrent(pq, lastCur) {
		return false // remote is fine, nothing changed
	}
	if err := e.subsonic.SavePlayQueue(lastIDs, lastCur, nil); err != nil {
		log.Printf("Re-push failed: %v", err)
		return false
	}
	e.mu.Lock()
	e.lastPushTS = time.Now()
	e.mu.Unlock()
	log.Printf("Re-pushed %d songs; foreign changes restored (resurrection of last MPD queue)", len(lastIDs))
	return true
}

// matchesRemoteCurrent reports whether pq's current entry corresponds to id.
func (e *SyncEngine) matchesRemoteCurrent(pq *PlayQueue, id string) bool {
	if id == "" {
		return true
	}
	for _, en := range pq.Entry {
		if en.ID == id {
			return pq.Current == "" || pq.Current == id
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		if counts[s] == 0 {
			return false
		}
		counts[s]--
	}
	return true
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
	f := e.mpd.findFile(entry.Title, entry.Artist, entry.Album,
		intPtr(entry.Track), intPtr(entry.DiscNumber), intPtr(int(entry.Duration)))
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
