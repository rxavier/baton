package main

// baton — bridge MPD's play queue to Navidrome's Subsonic play queue
// (savePlayQueue / getPlayQueue) so you can continue listening on any device,
// Spotify-style handoff across MPD and Subsonic clients.
//
// Single-file Go binary. See PLAN.md and README.md.

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultConfig is the template written to the user config path on first run.
// Keep it in sync with .env.example.
const defaultConfig = `# baton configuration (generated on first run — edit and restart baton)
#
# baton never reads your music files. It matches songs by tag through MPD and
# Navidrome, so you only configure how to reach them.

# Navidrome account baton authenticates as. Use the SAME account as your music
# clients (Feishin, Tempo, Tempus, Resonus, Malp...) so they share one queue.
NAVIDROME_USER=
NAVIDROME_PASSWORD=
NAVIDROME_URL=http://127.0.0.1:4533
# Set to 1 if your server only supports legacy plaintext auth (not Navidrome).
NAVIDROME_LEGACY_AUTH=0

# MPD host/port/password.
MPD_HOST=127.0.0.1
MPD_PORT=6600
MPD_PASSWORD=

# Poll interval (seconds) and sync mode: auto|manual|push|pull.
POLL_INTERVAL=5
MODE=auto

# Status API + web UI listen address.
BATON_LISTEN=127.0.0.1:8080
# Optional bearer token protecting /api/*. Leave empty for no auth.
BATON_API_TOKEN=
`

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type Config struct {
	NavidromeURL    string
	NavidromeUser   string
	NavidromePass   string
	NavidromeLegacy bool
	MPDHost         string
	MPDPort         int
	MPDPassword     string
	PollInterval    time.Duration
	Mode            string // auto | manual | push | pull
	Listen          string
	APIToken        string
	Verbose         bool
}

// readDotenv loads a dotenv-style config file (KEY=VALUE lines, # comments)
// into a map. A missing file is not an error; an unreadable one is.
func readDotenv(path string) (map[string]string, error) {
	cfg := map[string]string{}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		// strip surrounding quotes
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		cfg[key] = val
	}
	return cfg, nil
}

// findConfigPath resolves the config file to load. Precedence: explicit
// --env-file flag, then $BATON_ENV_FILE, then the user config path if it
// already exists. Returns "" when no config file is in play.
func findConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if v := os.Getenv("BATON_ENV_FILE"); v != "" {
		return v
	}
	if p := userConfigPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// userConfigPath returns ~/.config/baton/baton.env (XDG-aware).
func userConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "baton", "baton.env")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "baton", "baton.env")
}

// writeDefaultConfig seeds a commented template the user can edit.
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0o600)
}

// interactive reports whether stdin looks like a terminal (vs a daemon/pipe).
func interactive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func loadConfig() Config {
	var envFile string
	flag.StringVar(&envFile, "env-file", "", "path to a dotenv-style config file (KEY=VALUE)")
	flag.StringVar(&cURL, "navidrome-url", "", "Navidrome Subsonic base URL")
	flag.StringVar(&cUser, "navidrome-user", "", "Navidrome username")
	flag.StringVar(&cPass, "navidrome-password", "", "Navidrome password")
	flag.BoolVar(&cLegacy, "legacy-auth", false, "use plaintext p= auth")
	flag.StringVar(&cHost, "mpd-host", "", "MPD host")
	mpdPort := flag.Int("mpd-port", 0, "MPD port")
	flag.StringVar(&cMpdPass, "mpd-password", "", "MPD password")
	poll := flag.Float64("poll-interval", 0, "poll interval seconds")
	flag.StringVar(&cMode, "mode", "", "sync mode: auto|manual|push|pull")
	flag.StringVar(&cListen, "listen", "", "status API + web UI listen address")
	flag.StringVar(&cToken, "api-token", "", "optional bearer token for the API")
	flag.BoolVar(&cVerbose, "verbose", false, "verbose debug logging")
	flag.Parse()

	cfg, err := readDotenv(findConfigPath(envFile))
	if err != nil {
		log.Fatalf("cannot read config file: %v", err)
	}

	// Precedence: explicit flag > environment > config file > built-in default.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	pick := func(flagName, envKey, flagVal, def string) string {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		if v := cfg[envKey]; v != "" {
			return v
		}
		return def
	}
	pickBool := func(flagName, envKey string) bool {
		if set[flagName] {
			return true
		}
		if v := os.Getenv(envKey); v != "" {
			return v == "1" || strings.EqualFold(v, "true")
		}
		if v := cfg[envKey]; v != "" {
			return v == "1" || strings.EqualFold(v, "true")
		}
		return false
	}
	pickInt := func(flagName, envKey string, flagVal int, def int) int {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return mustInt(v)
		}
		if v := cfg[envKey]; v != "" {
			return mustInt(v)
		}
		return def
	}
	pickFloat := func(flagName, envKey string, flagVal float64, def float64) float64 {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return mustFloat(v)
		}
		if v := cfg[envKey]; v != "" {
			return mustFloat(v)
		}
		return def
	}

	c := Config{}
	c.NavidromeURL = pick("navidrome-url", "NAVIDROME_URL", cURL, "http://127.0.0.1:4533")
	c.NavidromeUser = pick("navidrome-user", "NAVIDROME_USER", cUser, "")
	c.NavidromePass = pick("navidrome-password", "NAVIDROME_PASSWORD", cPass, "")
	c.NavidromeLegacy = pickBool("legacy-auth", "NAVIDROME_LEGACY_AUTH")
	c.MPDHost = pick("mpd-host", "MPD_HOST", cHost, "127.0.0.1")
	c.MPDPort = pickInt("mpd-port", "MPD_PORT", *mpdPort, 6600)
	c.MPDPassword = pick("mpd-password", "MPD_PASSWORD", cMpdPass, "")
	c.PollInterval = time.Duration(pickFloat("poll-interval", "POLL_INTERVAL", *poll, 5) * float64(time.Second))
	c.Mode = pick("mode", "MODE", cMode, "auto")
	c.Listen = pick("listen", "BATON_LISTEN", cListen, "127.0.0.1:8080")
	c.APIToken = pick("api-token", "BATON_API_TOKEN", cToken, "")
	c.Verbose = pickBool("verbose", "VERBOSE")
	return c
}

var (
	cURL, cUser, cPass, cHost, cMpdPass, cMode, cListen, cToken string
	cLegacy, cVerbose                                           bool
)

func mustInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		log.Fatalf("invalid int: %s", s)
	}
	return i
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Fatalf("invalid float: %s", s)
	}
	return f
}

// ---------------------------------------------------------------------------
// Subsonic client
// ---------------------------------------------------------------------------

type Subsonic struct {
	baseURL  string
	user     string
	password string
	legacy   bool
	client   *http.Client
}

func NewSubsonic(baseURL, user, password string, legacy bool) *Subsonic {
	return &Subsonic{
		baseURL:  strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		legacy:   legacy,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Subsonic) params(extra url.Values) url.Values {
	p := url.Values{}
	if s.legacy {
		p.Set("u", s.user)
		p.Set("p", s.password)
	} else {
		saltBytes := make([]byte, 8)
		rand.Read(saltBytes)
		salt := hex.EncodeToString(saltBytes)
		sum := md5.Sum([]byte(s.password + salt))
		p.Set("u", s.user)
		p.Set("t", hex.EncodeToString(sum[:]))
		p.Set("s", salt)
	}
	for k, vs := range extra {
		for _, v := range vs {
			p.Add(k, v)
		}
	}
	p.Set("f", "json")
	p.Set("v", "1.16.1")
	p.Set("c", "baton")
	return p
}

func (s *Subsonic) get(method string, extra url.Values) (map[string]any, error) {
	u := fmt.Sprintf("%s/rest/%s.view", s.baseURL, method) + "?" + s.params(extra).Encode()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		SubsonicResponse struct {
			Status        string         `json:"status"`
			Error         map[string]any `json:"error"`
			PlayQueue     *PlayQueue     `json:"playQueue"`
			SearchResult3 *struct {
				Song []map[string]any `json:"song"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("%s: bad json: %w", method, err)
	}
	sr := wrapper.SubsonicResponse
	if sr.Status != "ok" {
		return nil, fmt.Errorf("%s failed: %v", method, sr.Error)
	}
	out := map[string]any{"status": "ok"}
	if sr.PlayQueue != nil {
		out["playQueue"] = sr.PlayQueue
	}
	if sr.SearchResult3 != nil {
		out["searchResult3"] = sr.SearchResult3
	}
	return out, nil
}

func (s *Subsonic) Ping() bool {
	_, err := s.get("ping", nil)
	return err == nil
}

func (s *Subsonic) SavePlayQueue(ids []string, current string, position *int) error {
	if len(ids) == 0 {
		return nil
	}
	extra := url.Values{}
	for _, id := range ids {
		extra.Add("id", id)
	}
	if current != "" {
		extra.Set("current", current)
	}
	if position != nil {
		extra.Set("position", strconv.Itoa(*position))
	}
	_, err := s.get("savePlayQueue", extra)
	return err
}

func (s *Subsonic) GetPlayQueue() (*PlayQueue, error) {
	out, err := s.get("getPlayQueue", nil)
	if err != nil {
		return nil, err
	}
	pq, ok := out["playQueue"].(*PlayQueue)
	if !ok {
		return nil, nil
	}
	return pq, nil
}

type PlayQueue struct {
	Changed        string           `json:"changed"`
	Username       string           `json:"username"`
	Current        string           `json:"current"`
	Position       int64            `json:"position"`
	PositionMillis int64            `json:"positionMilliseconds"`
	Entry          []PlayQueueEntry `json:"entry"`
}

type PlayQueueEntry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	Track      int    `json:"track"`
	DiscNumber int    `json:"discNumber"`
}

func (s *Subsonic) ResolveID(title, artist, album string, track, disc *int) (string, error) {
	if title == "" {
		return "", nil
	}
	extra := url.Values{}
	extra.Set("query", title)
	extra.Set("songCount", "50")
	extra.Set("artistCount", "0")
	extra.Set("albumCount", "0")
	out, err := s.get("search3", extra)
	if err != nil {
		return "", err
	}
	sr, ok := out["searchResult3"].(*struct {
		Song []map[string]any `json:"song"`
	})
	if !ok || sr == nil {
		return "", nil
	}
	for _, song := range sr.Song {
		if match(song, title, artist, album, track, disc) {
			if id, ok := song["id"].(string); ok {
				return id, nil
			}
		}
	}
	// fallback: album+title only (artist normalization can differ)
	for _, song := range sr.Song {
		if album != "" {
			if songTitle, _ := song["title"].(string); songTitle == title {
				if songAlbum, _ := song["album"].(string); songAlbum == album {
					if id, ok := song["id"].(string); ok {
						return id, nil
					}
				}
			}
		}
	}
	return "", nil
}

func match(song map[string]any, title, artist, album string, track, disc *int) bool {
	if t, _ := song["title"].(string); t != title {
		return false
	}
	if artist != "" {
		if a, _ := song["artist"].(string); a != artist {
			return false
		}
	}
	if album != "" {
		if a, _ := song["album"].(string); a != album {
			return false
		}
	}
	if track != nil {
		if t, _ := normInt(song["track"]); t != *track {
			return false
		}
	}
	if disc != nil {
		if d, _ := normInt(song["discNumber"]); d != *disc {
			return false
		}
	}
	return true
}

func normInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return int(f), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// MPD client (minimal TCP protocol)
// ---------------------------------------------------------------------------

type MPD struct {
	host     string
	port     int
	password string
	conn     net.Conn
	reader   *bufio.Reader
	mu       sync.Mutex
}

func NewMPD(host string, port int, password string) *MPD {
	return &MPD{host: host, port: port, password: password}
}

func (m *MPD) connect() error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(m.host, strconv.Itoa(m.port)), 10*time.Second)
	if err != nil {
		return err
	}
	m.conn = conn
	m.reader = bufio.NewReader(conn)
	// read MPD version banner
	line, err := m.readLine()
	if err != nil {
		conn.Close()
		return err
	}
	if !strings.HasPrefix(line, "OK MPD") {
		conn.Close()
		return fmt.Errorf("unexpected MPD banner: %q", line)
	}
	if m.password != "" {
		if _, err := m.sendCommandLocked("password", m.password); err != nil {
			conn.Close()
			return err
		}
	}
	return nil
}

func (m *MPD) readLine() (string, error) {
	line, err := m.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (m *MPD) disconnect() {
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
}

func (m *MPD) ensureConnected() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		if err := m.connect(); err != nil {
			return err
		}
	}
	return nil
}

// command sends a command and parses the response into a list of entries.
// Each entry is a map of lowercased key -> value. Entries are split on the
// "file:" key (song-list commands) or on a blank line.
func (m *MPD) command(args ...string) ([]map[string]any, error) {
	if err := m.ensureConnected(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendCommandLocked(args...)
}

// sendCommandLocked sends a command and parses the response. Caller must hold m.mu.
func (m *MPD) sendCommandLocked(args ...string) ([]map[string]any, error) {
	cmd := strings.Join(args, " ")
	if _, err := io.WriteString(m.conn, cmd+"\n"); err != nil {
		m.disconnect()
		return nil, err
	}
	var entries []map[string]any
	var cur map[string]any
	for {
		line, err := m.readLine()
		if err != nil {
			m.disconnect()
			return nil, err
		}
		if line == "OK" {
			break
		}
		if strings.HasPrefix(line, "ACK") {
			m.disconnect()
			return nil, fmt.Errorf("MPD %s", line)
		}
		if line == "" {
			if cur != nil {
				entries = append(entries, cur)
				cur = nil
			}
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		val := strings.TrimSpace(line[idx+1:])
		if key == "file" && cur != nil {
			// song boundary
			entries = append(entries, cur)
			cur = map[string]any{}
		}
		if cur == nil {
			cur = map[string]any{}
		}
		cur[key] = val
	}
	if cur != nil {
		entries = append(entries, cur)
	}
	return entries, nil
}

func (m *MPD) playlistinfo() []map[string]any {
	entries, err := m.command("playlistinfo")
	if err != nil {
		return nil
	}
	return entries
}

func (m *MPD) status() map[string]any {
	entries, err := m.command("status")
	if err != nil {
		return map[string]any{}
	}
	if len(entries) == 0 {
		return map[string]any{}
	}
	return entries[0]
}

func (m *MPD) currentsong() map[string]any {
	entries, err := m.command("currentsong")
	if err != nil {
		return map[string]any{}
	}
	if len(entries) == 0 {
		return map[string]any{}
	}
	return entries[len(entries)-1]
}

func (m *MPD) state() string {
	st, _ := m.status()["state"].(string)
	return st
}

func (m *MPD) queueFiles() []string {
	var files []string
	for _, s := range m.playlistinfo() {
		if f, ok := s["file"].(string); ok {
			files = append(files, f)
		}
	}
	return files
}

func (m *MPD) elapsedMs() *int {
	el, ok := m.status()["elapsed"].(string)
	if !ok {
		return nil
	}
	f, err := strconv.ParseFloat(el, 64)
	if err != nil {
		return nil
	}
	ms := int(f * 1000)
	return &ms
}

func (m *MPD) clear() {
	m.command("clear")
}

func (m *MPD) add(file string) {
	m.command("add", quoteArg(file))
}

func (m *MPD) play(pos int) {
	m.command("play", strconv.Itoa(pos))
}

func (m *MPD) stop() {
	m.command("stop")
}

func (m *MPD) pause(on bool) {
	if on {
		m.command("pause", "1")
	} else {
		m.command("pause", "0")
	}
}

func (m *MPD) seekcur(ms int) {
	// MPD's seekcur takes a TIME in seconds (float), not milliseconds.
	secs := strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64)
	m.command("seekcur", secs)
}

func (m *MPD) find(tags map[string]string) []map[string]any {
	if len(tags) == 0 {
		return nil
	}
	args := []string{"find"}
	for k, v := range tags {
		args = append(args, k, quoteArg(v))
	}
	entries, err := m.command(args...)
	if err != nil {
		return nil
	}
	return entries
}

func (m *MPD) search(tags map[string]string) []map[string]any {
	if len(tags) == 0 {
		return nil
	}
	args := []string{"search"}
	for k, v := range tags {
		args = append(args, k, quoteArg(v))
	}
	entries, err := m.command(args...)
	if err != nil {
		return nil
	}
	return entries
}

func quoteArg(s string) string {
	// MPD args can be quoted; escape backslash and quote
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

func (m *MPD) findFile(title, artist, album string, track, disc *int) string {
	candidates := m.find(map[string]string{"album": album, "title": title})
	if len(candidates) == 0 && artist != "" {
		candidates = m.find(map[string]string{"album": album, "artist": artist, "title": title})
	}
	if len(candidates) == 0 {
		candidates = m.search(map[string]string{"title": title, "album": album})
	}
	for _, s := range candidates {
		if track != nil {
			if t, ok := normInt(s["track"]); ok && t != *track {
				continue
			}
		}
		if disc != nil {
			if d, ok := normInt(s["disc"]); ok && d != *disc {
				continue
			}
		}
		if f, ok := s["file"].(string); ok {
			return f
		}
	}
	if len(candidates) > 0 {
		if f, ok := candidates[0]["file"].(string); ok {
			return f
		}
	}
	return ""
}

func (m *MPD) loadFiles(files []string, currentIndex *int, wasPaused bool, positionMs *int) {
	if len(files) == 0 {
		return
	}
	m.clear()
	for _, f := range files {
		m.add(f)
	}
	if currentIndex != nil && *currentIndex >= 0 && *currentIndex < len(files) {
		m.play(*currentIndex)
		if positionMs != nil && *positionMs > 0 {
			m.seekcur(*positionMs)
		}
		m.settle(wasPaused, positionMs)
	}
}

func (m *MPD) setCurrentIndex(index int, wasPaused bool, positionMs *int) {
	m.play(index)
	if positionMs != nil && *positionMs > 0 {
		m.seekcur(*positionMs)
	}
	m.settle(wasPaused, positionMs)
}

// settle ends the cursor-set/seek sequence in the right state:
//   - position restored -> must stay paused (stop would discard the position)
//   - MPD was paused     -> back to pause
//   - otherwise          -> stop
func (m *MPD) settle(wasPaused bool, positionMs *int) {
	if positionMs != nil && *positionMs > 0 {
		m.pause(true)
	} else if wasPaused {
		m.pause(true)
	} else {
		m.stop()
	}
}

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
	return &SyncEngine{
		subsonic: sub,
		mpd:      m,
		cfg:      cfg,
		fpToID:   map[string]string{},
		idToFile: map[string]string{},
	}
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
	cfg    Config
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
	MPDState      string   `json:"mpd_state"`
	CurrentSong   string   `json:"current_song"`
	PositionMs    *int     `json:"position_ms"`
	Queue         []string `json:"queue"`
	RemoteChanged string   `json:"remote_changed"`
	LastPushTS    string   `json:"last_push_ts"`
	Mode          string   `json:"mode"`
}

func (a *APIServer) status(w http.ResponseWriter, r *http.Request) {
	st := statusResponse{
		MPDState:    a.engine.mpd.state(),
		CurrentSong: strVal(a.engine.mpd.currentsong(), "file"),
		PositionMs:  a.engine.mpd.elapsedMs(),
		Queue:       a.engine.mpd.queueFiles(),
		Mode:        a.cfg.Mode,
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
	}
	writeJSON(w, st)
}

func (a *APIServer) push(w http.ResponseWriter, r *http.Request) {
	a.engine.pushQueue()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (a *APIServer) pull(w http.ResponseWriter, r *http.Request) {
	a.engine.pullQueue()
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if cfg.NavidromeUser == "" || cfg.NavidromePass == "" {
		if interactive() && findConfigPath("") == "" {
			p := userConfigPath()
			if err := writeDefaultConfig(p); err != nil {
				log.Fatalf("could not write default config to %s: %v", p, err)
			}
			log.Fatalf("Created a default config at %s. Edit it to set "+
				"NAVIDROME_USER / NAVIDROME_PASSWORD (and optionally the port), "+
				"then run baton again.", p)
		}
		log.Fatal("NAVIDROME_USER and NAVIDROME_PASSWORD are required (set them in " +
			"the config file, as environment variables, or with --env-file)")
	}
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
	api := &APIServer{engine: engine, cfg: cfg, token: cfg.APIToken}
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

	log.Printf("Starting baton (mode=%s, poll=%s)", cfg.Mode, cfg.PollInterval)
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
			switch cfg.Mode {
			case "auto", "push":
				engine.pushQueue()
			}
			if cfg.Mode == "auto" || cfg.Mode == "pull" {
				engine.pullQueue()
			}
		}
	}
}
