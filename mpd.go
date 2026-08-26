package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
