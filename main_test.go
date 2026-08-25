package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealMPD(t *testing.T) {
	m := NewMPD("127.0.0.1", 6600, "")
	if err := m.ensureConnected(); err != nil {
		t.Skipf("MPD not reachable: %v", err)
	}
	defer m.disconnect()

	state := m.state()
	if state == "" {
		t.Error("empty MPD state")
	} else {
		t.Logf("MPD state: %s", state)
	}

	files := m.queueFiles()
	if len(files) == 0 {
		t.Log("MPD queue empty")
	} else {
		t.Logf("MPD queue has %d songs; first=%s", len(files), files[0])
	}

	cur := m.currentsong()
	if f, ok := cur["file"].(string); ok {
		t.Logf("current: %s", f)
	} else {
		t.Log("no current song")
	}
}

func TestParseChanged(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"2026-08-25T12:00:00.000Z":          true,
		"2026-08-25T12:00:00Z":              true,
		"2026-08-25T12:00:00.0000000+00:00": true,
		"2026-08-25 12:00:00":               true,
		"not-a-date":                        false,
	}
	for in, want := range cases {
		got := parseChanged(in)
		if (got != nil) != want {
			t.Errorf("parseChanged(%q) = %v, want non-nil=%v", in, got, want)
		}
	}
	if got := parseChanged("2026-08-25T12:00:00Z"); got == nil || got.UTC().Unix() != 1787659200 {
		t.Errorf("epoch mismatch: %v", got)
	}
}

func TestParseSig(t *testing.T) {
	ids, cur := parseSig("[a b c]\x00b")
	if len(ids) != 3 || ids[1] != "b" || cur != "b" {
		t.Errorf("parseSig got ids=%v cur=%q", ids, cur)
	}
}

func TestPositionFor(t *testing.T) {
	pq := &PlayQueue{Current: "x", Position: 90, PositionMillis: 0}
	got := positionFor(pq, 2)
	if got == nil || *got != 90000 {
		t.Errorf("expected 90000ms, got %v", got)
	}
	pq2 := &PlayQueue{Current: "x", PositionMillis: 12345}
	got2 := positionFor(pq2, 0)
	if got2 == nil || *got2 != 12345 {
		t.Errorf("expected 12345ms, got %v", got2)
	}
	got3 := positionFor(pq, -1)
	if got3 != nil {
		t.Errorf("expected nil for index -1, got %v", *got3)
	}
}

var _ = time.Second

func TestReadDotenv(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/baton.env"
	os.WriteFile(p, []byte(`
# comment
NAVIDROME_URL=http://nd:4533
NAVIDROME_USER="quoted user"
MPD_PORT=7777
BATON_LISTEN=127.0.0.1:9999
`), 0o600)
	cfg, err := readDotenv(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["NAVIDROME_URL"] != "http://nd:4533" {
		t.Errorf("url: %q", cfg["NAVIDROME_URL"])
	}
	if cfg["NAVIDROME_USER"] != "quoted user" {
		t.Errorf("user (quotes stripped): %q", cfg["NAVIDROME_USER"])
	}
	if cfg["MPD_PORT"] != "7777" {
		t.Errorf("port: %q", cfg["MPD_PORT"])
	}
	if _, ok := cfg["BATON_LISTEN"]; !ok {
		t.Error("missing BATON_LISTEN")
	}
}

func TestReadDotenvMissing(t *testing.T) {
	cfg, err := readDotenv(t.TempDir() + "/nope.env")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != 0 {
		t.Errorf("expected empty map, got %v", cfg)
	}
}

func TestFindConfigPath(t *testing.T) {
	// explicit flag wins
	if p := findConfigPath("/explicit.env"); p != "/explicit.env" {
		t.Errorf("flag path: %q", p)
	}
	// env var wins over user path
	t.Setenv("BATON_ENV_FILE", "/env-var.env")
	if p := findConfigPath(""); p != "/env-var.env" {
		t.Errorf("env var path: %q", p)
	}
	// user config path used if it exists
	dir := t.TempDir()
	existing := dir + "/baton.env"
	os.WriteFile(existing, []byte("A=1\n"), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir+"/xdg")
	os.MkdirAll(dir+"/xdg/baton", 0o755)
	os.WriteFile(dir+"/xdg/baton/baton.env", []byte("B=2\n"), 0o600)
	t.Setenv("BATON_ENV_FILE", "")
	if p := findConfigPath(""); p != dir+"/xdg/baton/baton.env" {
		t.Errorf("user config path: %q", p)
	}
	// nothing found -> ""
	t.Setenv("XDG_CONFIG_HOME", dir+"/empty-xdg")
	if p := findConfigPath(""); p != "" {
		t.Errorf("expected no config, got %q", p)
	}
}

func TestUserConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if p := userConfigPath(); p != "/tmp/xdg/baton/baton.env" {
		t.Errorf("xdg path: %q", p)
	}
}

func TestWriteDefaultConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "baton", "baton.env")
	if err := writeDefaultConfig(p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "NAVIDROME_USER=") {
		t.Errorf("default config missing NAVIDROME_USER:\n%s", data)
	}
	if !strings.Contains(string(data), "BATON_LISTEN=127.0.0.1:8080") {
		t.Errorf("default config missing BATON_LISTEN:\n%s", data)
	}
}
