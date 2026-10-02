package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fake MPD server
// ---------------------------------------------------------------------------

type fakeSong struct {
	file, title, artist, album string
	track, disc                int
	dur                        int
}

type fakeMPD struct {
	mu       sync.Mutex
	library  []fakeSong // music database (find/search target)
	queue    []fakeSong // current playlist
	cur      int        // current index, -1 = none
	state    string
	position int // ms
	listener net.Listener
	commands []string
	password string // if set, requires `password <pw>` first
	authed   bool
}

func newFakeMPD(songs []fakeSong) (*fakeMPD, error) {
	f := &fakeMPD{library: songs, queue: append([]fakeSong(nil), songs...), cur: -1, state: "stop"}
	return f, f.listen()
}

func (f *fakeMPD) listen() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	f.listener = ln
	go f.serve()
	return nil
}

func (f *fakeMPD) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeMPD) handle(conn net.Conn) {
	defer conn.Close()
	fmt.Fprintf(conn, "OK MPD 0.24.0\n")
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		parts := strings.Fields(line)
		cmd := parts[0]
		var resp string
		switch cmd {
		case "password":
			f.mu.Lock()
			if f.password != "" && len(parts) == 2 && parts[1] == f.password {
				f.authed = true
				resp = "OK\n"
			} else if f.password == "" {
				resp = "OK\n"
			} else {
				resp = "ACK wrong password\n"
			}
			f.mu.Unlock()
		case "status":
			f.mu.Lock()
			ok := f.password == "" || f.authed
			f.mu.Unlock()
			if !ok {
				resp = "ACK not authorized\n"
			} else {
				resp = f.cmdStatus()
			}
		case "playlistinfo":
			resp = f.cmdSongs("playlistinfo")
		case "currentsong":
			resp = f.cmdCurrent()
		case "find", "search":
			resp = f.cmdFind(parts)
		case "clear":
			f.mu.Lock()
			f.queue = nil
			f.cur = -1
			f.mu.Unlock()
			resp = "OK\n"
		case "add":
			file := unquote(strings.Join(parts[1:], " "))
			f.mu.Lock()
			f.queue = append(f.queue, fakeSong{file: file})
			f.mu.Unlock()
			resp = "OK\n"
		case "play":
			if len(parts) > 1 {
				i, _ := strconv.Atoi(parts[1])
				f.mu.Lock()
				f.cur = i
				f.state = "play"
				f.position = 0
				f.mu.Unlock()
			}
			resp = "OK\n"
		case "stop":
			f.mu.Lock()
			f.state = "stop"
			f.mu.Unlock()
			resp = "OK\n"
		case "pause":
			f.mu.Lock()
			f.state = "pause"
			f.mu.Unlock()
			resp = "OK\n"
		case "seekcur":
			if len(parts) > 1 {
				secs, _ := strconv.ParseFloat(parts[1], 64)
				f.mu.Lock()
				f.position = int(secs * 1000)
				f.mu.Unlock()
			}
			resp = "OK\n"
		case "ping":
			resp = "OK\n"
		default:
			resp = "ACK unknown command\n"
		}
		conn.Write([]byte(resp))
	}
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func (f *fakeMPD) cmdStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	elapsed := float64(f.position) / 1000.0
	return fmt.Sprintf("state: %s\nelapsed: %.3f\nOK\n", f.state, elapsed)
}

func (f *fakeMPD) cmdSongs(_ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, s := range f.queue {
		fmt.Fprintf(&b, "file: %s\n", s.file)
		if s.title != "" {
			fmt.Fprintf(&b, "Title: %s\n", s.title)
		}
		if s.artist != "" {
			fmt.Fprintf(&b, "Artist: %s\n", s.artist)
		}
		if s.album != "" {
			fmt.Fprintf(&b, "Album: %s\n", s.album)
		}
		if s.track != 0 {
			fmt.Fprintf(&b, "Track: %d\n", s.track)
		}
		if s.disc != 0 {
			fmt.Fprintf(&b, "Disc: %d\n", s.disc)
		}
		if s.dur != 0 {
			fmt.Fprintf(&b, "Time: %d\n", s.dur)
		}
	}
	b.WriteString("OK\n")
	return b.String()
}

func (f *fakeMPD) cmdCurrent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur < 0 || f.cur >= len(f.queue) {
		return "OK\n"
	}
	s := f.queue[f.cur]
	return fmt.Sprintf("file: %s\nTitle: %s\nArtist: %s\nAlbum: %s\nOK\n", s.file, s.title, s.artist, s.album)
}

func (f *fakeMPD) cmdFind(parts []string) string {
	// find type "value" type "value"... we just match title/album/artist loosely
	f.mu.Lock()
	defer f.mu.Unlock()
	need := map[string]string{}
	for i := 1; i+1 < len(parts); i += 2 {
		need[parts[i]] = unquote(parts[i+1])
	}
	var b strings.Builder
	for _, s := range f.library {
		match := true
		for k, v := range need {
			switch k {
			case "title":
				if s.title != v {
					match = false
				}
			case "album":
				if s.album != v {
					match = false
				}
			case "artist":
				if s.artist != v {
					match = false
				}
			}
		}
		if !match {
			continue
		}
		fmt.Fprintf(&b, "file: %s\nTitle: %s\nArtist: %s\nAlbum: %s\nTrack: %d\nDisc: %d\n",
			s.file, s.title, s.artist, s.album, s.track, s.disc)
		if s.dur != 0 {
			fmt.Fprintf(&b, "Time: %d\n", s.dur)
		}
	}
	b.WriteString("OK\n")
	return b.String()
}

func (f *fakeMPD) lastCommands(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.commands) {
		return f.commands
	}
	return f.commands[len(f.commands)-n:]
}

func (f *fakeMPD) queueFilesNow() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.queue))
	for i, s := range f.queue {
		out[i] = s.file
	}
	return out
}

func (f *fakeMPD) curIndex() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

func (f *fakeMPD) positionNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.position
}

func (f *fakeMPD) setState(s string) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}

func (f *fakeMPD) stateNow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeMPD) close() {
	f.listener.Close()
}

// setPassword enables password auth on the fake server. Returns the host/port
// for a NewMPD with the matching password.
func (f *fakeMPD) setPassword(pw string) (string, int) {
	f.mu.Lock()
	f.password = pw
	f.mu.Unlock()
	host, portStr, _ := net.SplitHostPort(f.listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// clearQueue empties the playlist but keeps the library intact.
func (f *fakeMPD) clearQueue() {
	f.mu.Lock()
	f.queue = nil
	f.cur = -1
	f.state = "stop"
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Fake Navidrome
// ---------------------------------------------------------------------------

type fakeNavidrome struct {
	mu        sync.Mutex
	saved     *PlayQueue
	searchMap map[string][]PlayQueueEntry // query title -> entries
	changed   string
	getCalls  int
}

func newFakeNavidrome() *fakeNavidrome {
	return &fakeNavidrome{searchMap: map[string][]PlayQueueEntry{}}
}

func (n *fakeNavidrome) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/", func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/"), ".view")
		q := r.URL.Query()
		var out map[string]any
		n.mu.Lock()
		switch method {
		case "ping":
			out = map[string]any{"status": "ok"}
		case "savePlayQueue":
			var ids []string
			for _, id := range q["id"] {
				ids = append(ids, id)
			}
			cur := q.Get("current")
			var pos int64
			if p := q.Get("position"); p != "" {
				pos, _ = strconv.ParseInt(p, 10, 64)
			}
			n.saved = &PlayQueue{
				Changed:  time.Now().UTC().Format(time.RFC3339Nano),
				Current:  cur,
				Position: pos,
				Entry:    n.entriesForIDs(ids),
			}
			out = map[string]any{"status": "ok"}
		case "getPlayQueue":
			n.getCalls++
			if n.saved == nil {
				n.mu.Unlock()
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "ok", "version": "1.16.1"}})
				return
			}
			pq := *n.saved
			if n.changed != "" {
				pq.Changed = n.changed
			}
			out = map[string]any{"status": "ok", "playQueue": pq}
		case "search3":
			query := q.Get("query")
			var songs []map[string]any
			for _, e := range n.searchMap[query] {
				songs = append(songs, map[string]any{
					"id":         e.ID,
					"title":      e.Title,
					"artist":     e.Artist,
					"album":      e.Album,
					"track":      e.Track,
					"discNumber": e.DiscNumber,
					"duration":   e.Duration,
				})
			}
			out = map[string]any{"status": "ok", "searchResult3": map[string]any{"song": songs}}
		default:
			out = map[string]any{"status": "failed", "error": map[string]any{"message": "unknown"}}
		}
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": out})
	})
	return mux
}

func (n *fakeNavidrome) entriesForIDs(ids []string) []PlayQueueEntry {
	var out []PlayQueueEntry
	for _, id := range ids {
		for _, list := range n.searchMap {
			for _, e := range list {
				if e.ID == id {
					out = append(out, e)
					break
				}
			}
		}
	}
	return out
}

func (n *fakeNavidrome) setSaved(pq *PlayQueue) {
	n.mu.Lock()
	n.saved = pq
	n.mu.Unlock()
}

func (n *fakeNavidrome) getSaved() *PlayQueue {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.saved
}

func (n *fakeNavidrome) setSearch(title string, entries ...PlayQueueEntry) {
	n.mu.Lock()
	n.searchMap[title] = entries
	n.mu.Unlock()
}

func (n *fakeNavidrome) setChanged(ts string) {
	n.mu.Lock()
	n.changed = ts
	n.mu.Unlock()
}

func (n *fakeNavidrome) getCallCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.getCalls
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testEnv(t *testing.T, songs []fakeSong) (*SyncEngine, *fakeMPD, *fakeNavidrome, *Subsonic) {
	t.Helper()
	fmpd, err := newFakeMPD(songs)
	if err != nil {
		t.Fatalf("fake mpd: %v", err)
	}
	t.Cleanup(fmpd.close)

	nav := newFakeNavidrome()
	ts := httptest.NewServer(nav.handler())
	t.Cleanup(ts.Close)

	cfg := Config{PollInterval: time.Second}
	sub := NewSubsonic(ts.URL, "user", "pass", false)
	host, portStr, err := net.SplitHostPort(fmpd.listener.Addr().String())
	if err != nil {
		t.Fatalf("split fake mpd addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	mpd := NewMPD(host, port, "")
	if err := mpd.ensureConnected(); err != nil {
		t.Fatalf("connect fake mpd: %v", err)
	}
	t.Cleanup(mpd.disconnect)

	engine := NewSyncEngine(sub, mpd, cfg)
	return engine, fmpd, nav, sub
}

func entry(id, title, artist, album string, track, disc int) PlayQueueEntry {
	return PlayQueueEntry{ID: id, Title: title, Artist: artist, Album: album, Track: track, DiscNumber: disc}
}

func entryD(id, title, artist, album string, track, disc int, dur int) PlayQueueEntry {
	e := entry(id, title, artist, album, track, disc)
	e.Duration = float64(dur)
	return e
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

var testSongs = []fakeSong{
	{"a/01.flac", "One", "Artist", "AlbumX", 1, 1, 0},
	{"a/02.flac", "Two", "Artist", "AlbumX", 2, 1, 0},
	{"a/03.flac", "Three", "Artist", "AlbumX", 3, 1, 0},
}

func TestPushPush(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setSearch("Two", entry("id2", "Two", "Artist", "AlbumX", 2, 1))
	nav.setSearch("Three", entry("id3", "Three", "Artist", "AlbumX", 3, 1))

	engine.seedBaseline()
	engine.pushQueue()
	if nav.getSaved() != nil {
		t.Fatal("baseline must NOT push")
	}

	fmpd.mu.Lock()
	fmpd.queue = append(fmpd.queue, fakeSong{file: "a/04.flac", title: "Four", artist: "Artist", album: "AlbumX", track: 4, disc: 1})
	fmpd.mu.Unlock()
	nav.setSearch("Four", entry("id4", "Four", "Artist", "AlbumX", 4, 1))

	engine.pushQueue()
	saved := nav.getSaved()
	if saved == nil {
		t.Fatal("expected savePlayQueue after change")
	}
	if len(saved.Entry) != 4 || saved.Entry[3].ID != "id4" {
		t.Fatalf("unexpected saved entries: %+v", saved.Entry)
	}

	// no-op on identical signature
	engine.pushQueue()
	if c := len(nav.getSaved().Entry); c != 4 {
		t.Fatalf("redundant push: got %d", c)
	}
}

func TestPullLoad(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	fmpd.clearQueue()
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setSearch("Two", entry("id2", "Two", "Artist", "AlbumX", 2, 1))
	nav.setChanged(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed: time.Now().UTC().Format(time.RFC3339Nano),
		Current: "id2",
		Entry:   []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1), entry("id2", "Two", "Artist", "AlbumX", 2, 1)},
	})

	fmpd.setState("stop")
	engine.pullQueue()

	files := fmpd.queueFilesNow()
	if len(files) != 2 || files[0] != "a/01.flac" || files[1] != "a/02.flac" {
		t.Fatalf("unexpected loaded files: %v", files)
	}
	if fmpd.curIndex() != 1 {
		t.Fatalf("expected cursor at index 1, got %d", fmpd.curIndex())
	}
	if fmpd.stateNow() != "stop" {
		t.Fatalf("expected stop after load, got %s", fmpd.stateNow())
	}
}

func TestPullPositionRestore(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	fmpd.clearQueue()
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setChanged(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed:        time.Now().UTC().Format(time.RFC3339Nano),
		Current:        "id1",
		PositionMillis: 45000,
		Entry:          []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1)},
	})
	fmpd.setState("stop")
	engine.pullQueue()
	if fmpd.positionNow() != 45000 {
		t.Fatalf("expected seekcur to 45000ms, got %d", fmpd.positionNow())
	}
	if fmpd.stateNow() != "pause" {
		t.Fatalf("expected pause to hold restored position, got %s", fmpd.stateNow())
	}
}

func TestStalenessGuard(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setSearch("Two", entry("id2", "Two", "Artist", "AlbumX", 2, 1))
	nav.setSearch("Three", entry("id3", "Three", "Artist", "AlbumX", 3, 1))

	engine.seedBaseline()
	fmpd.mu.Lock()
	fmpd.queue = append(fmpd.queue, fakeSong{file: "a/04.flac", title: "Four", artist: "Artist", album: "AlbumX", track: 4, disc: 1})
	fmpd.mu.Unlock()
	nav.setSearch("Four", entry("id4", "Four", "Artist", "AlbumX", 4, 1))
	engine.pushQueue() // sets last_push_ts
	if nav.getSaved() == nil {
		t.Fatal("push should have saved")
	}

	// remote changed is older than last push -> skip
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	nav.setChanged(old)
	nav.setSaved(&PlayQueue{
		Changed: old,
		Current: "id2",
		Entry:   []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1), entry("id2", "Two", "Artist", "AlbumX", 2, 1)},
	})
	fmpd.setState("stop")
	before := fmpd.queueFilesNow()
	engine.pullQueue()
	after := fmpd.queueFilesNow()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("stale remote queue must NOT be loaded")
	}
}

func TestPullWhilePlaying(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setChanged(time.Now().UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed: time.Now().UTC().Format(time.RFC3339Nano),
		Current: "id1",
		Entry:   []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1)},
	})
	fmpd.setState("play")
	engine.pullQueue()
	if len(fmpd.queueFilesNow()) != 3 {
		t.Fatal("playing MPD must not be modified by pull")
	}
}

func TestCursorOnlyHandoff(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setSearch("Two", entry("id2", "Two", "Artist", "AlbumX", 2, 1))
	nav.setSearch("Three", entry("id3", "Three", "Artist", "AlbumX", 3, 1))
	engine.seedBaseline()

	fmpd.mu.Lock()
	fmpd.cur = 0
	fmpd.state = "stop"
	fmpd.mu.Unlock()

	nav.setChanged(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed: time.Now().UTC().Format(time.RFC3339Nano),
		Current: "id2",
		Entry:   []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1), entry("id2", "Two", "Artist", "AlbumX", 2, 1), entry("id3", "Three", "Artist", "AlbumX", 3, 1)},
	})
	engine.pullQueue()
	if fmpd.curIndex() != 1 {
		t.Fatalf("expected cursor move to index 1, got %d", fmpd.curIndex())
	}
	if len(fmpd.queueFilesNow()) != 3 {
		t.Fatal("queue should not be reloaded")
	}
}

func TestPushDurationMatch(t *testing.T) {
	// two copies of the same song (same tags), differing only in duration:
	// resolvePush must pick the ID whose duration matches the MPD song.
	dup := []fakeSong{
		{"b/01.flac", "Dupe", "Artist", "AlbumX", 1, 1, 200},
		{"b/02.flac", "Dupe", "Artist", "AlbumX", 1, 1, 301},
	}
	engine, _, nav, _ := testEnv(t, dup)
	nav.setSearch("Dupe", entryD("id-short", "Dupe", "Artist", "AlbumX", 1, 1, 200), entryD("id-long", "Dupe", "Artist", "AlbumX", 1, 1, 301))

	engine.pushQueue()
	saved := nav.getSaved()
	if saved == nil {
		t.Fatal("expected push")
	}
	if len(saved.Entry) != 2 || saved.Entry[0].ID != "id-short" || saved.Entry[1].ID != "id-long" {
		t.Fatalf("duration disambiguation failed: %+v", saved.Entry)
	}
}

func TestPullDurationMatch(t *testing.T) {
	// remote has the 301s copy; findFile must pick the local file with a
	// matching duration, not the first title match.
	dup := []fakeSong{
		{"b/01.flac", "Dupe", "Artist", "AlbumX", 1, 1, 200},
		{"b/02.flac", "Dupe", "Artist", "AlbumX", 1, 1, 301},
	}
	engine, fmpd, nav, _ := testEnv(t, dup)
	fmpd.clearQueue()
	nav.setChanged(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed: time.Now().UTC().Format(time.RFC3339Nano),
		Current: "id-long",
		Entry:   []PlayQueueEntry{entryD("id-long", "Dupe", "Artist", "AlbumX", 1, 1, 301)},
	})
	fmpd.setState("stop")
	engine.pullQueue()
	files := fmpd.queueFilesNow()
	if len(files) != 1 || files[0] != "b/02.flac" {
		t.Fatalf("expected b/02.flac (301s), got %v", files)
	}
}

func TestRemoteQueueCache(t *testing.T) {
	engine, _, nav, _ := testEnv(t, testSongs)
	nav.setChanged(time.Now().UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed: time.Now().UTC().Format(time.RFC3339Nano),
		Current: "id1",
		Entry:   []PlayQueueEntry{entry("id1", "One", "Artist", "AlbumX", 1, 1)},
	})

	// TTL short so the test can observe expiry
	engine.remoteTTL = 30 * time.Millisecond

	if _, err := engine.getRemoteQueue(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.getRemoteQueue(); err != nil {
		t.Fatal(err)
	}
	if c := nav.getCallCount(); c != 1 {
		t.Fatalf("expected 1 getPlayQueue within TTL, got %d", c)
	}

	time.Sleep(50 * time.Millisecond)
	if _, err := engine.getRemoteQueue(); err != nil {
		t.Fatal(err)
	}
	if c := nav.getCallCount(); c != 2 {
		t.Fatalf("expected refetch after TTL, got %d calls", c)
	}
}

func TestMPDPasswordAuth(t *testing.T) {
	f, err := newFakeMPD(testSongs)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	host, port := f.setPassword("pw123")

	mpd := NewMPD(host, port, "pw123")
	if err := mpd.ensureConnected(); err != nil {
		t.Fatalf("connect with correct password: %v", err)
	}
	defer mpd.disconnect()
	if n := len(mpd.queueFiles()); n != 3 {
		t.Fatalf("expected 3 songs after auth, got %d", n)
	}

	// wrong password must fail
	bad := NewMPD(host, port, "nope")
	if err := bad.ensureConnected(); err == nil {
		t.Fatal("expected connect failure with wrong password")
	}
}

func TestCursorHandoffPosition(t *testing.T) {
	engine, fmpd, nav, _ := testEnv(t, testSongs)
	nav.setSearch("One", entry("id1", "One", "Artist", "AlbumX", 1, 1))
	nav.setSearch("Two", entry("id2", "Two", "Artist", "AlbumX", 2, 1))
	nav.setSearch("Three", entry("id3", "Three", "Artist", "AlbumX", 3, 1))
	engine.seedBaseline()

	fmpd.mu.Lock()
	fmpd.cur = 0
	fmpd.state = "stop"
	fmpd.mu.Unlock()

	nav.setChanged(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	nav.setSaved(&PlayQueue{
		Changed:        time.Now().UTC().Format(time.RFC3339Nano),
		Current:        "id2",
		PositionMillis: 33000,
		Entry: []PlayQueueEntry{
			entry("id1", "One", "Artist", "AlbumX", 1, 1),
			entry("id2", "Two", "Artist", "AlbumX", 2, 1),
			entry("id3", "Three", "Artist", "AlbumX", 3, 1),
		},
	})
	engine.pullQueue()
	if fmpd.curIndex() != 1 {
		t.Fatalf("expected cursor move to index 1, got %d", fmpd.curIndex())
	}
	if fmpd.positionNow() != 33000 {
		t.Fatalf("expected seekcur to 33000ms, got %d", fmpd.positionNow())
	}
	if fmpd.stateNow() != "pause" {
		t.Fatalf("expected pause to hold position, got %s", fmpd.stateNow())
	}
}
